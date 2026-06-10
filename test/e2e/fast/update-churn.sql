-- Materialiser-chain stress: enough small UPDATEs against a tight
-- key range to build long per-page chains and trigger the
-- post-flush materialiser repeatedly. Exercises the
-- image_chain_limit / image_chain_payload_bytes_limit thresholds
-- (4.i) plus the post-flush trigger.
--
-- Sized for the "fast" tier: 500 rows × 8 passes ≈ 4k delta rows
-- across ~3 heap pages, well over the default chain limit (so the
-- materialiser fires) but small enough to finish in ~1 min given the
-- current per-page read cost.

\timing on
DROP TABLE IF EXISTS churn;
CREATE TABLE churn (
    id  INT PRIMARY KEY,
    cnt INT NOT NULL
);

INSERT INTO churn (id, cnt) SELECT g, 0 FROM generate_series(1, 500) g;

DO $$
BEGIN
    FOR i IN 1..8 LOOP
        UPDATE churn SET cnt = cnt + 1;
    END LOOP;
END $$;

SELECT count(*), sum(cnt) FROM churn;
SELECT count(*) FROM churn WHERE cnt = 8;

-- Deterministic verification (task 25): after 8 passes every row's cnt
-- must be exactly 8. A lost or stale delta in any chain shows up as a
-- changed digest or a cnt != 8 row.
SELECT md5(string_agg(id || ':' || cnt, ',' ORDER BY id)) AS digest FROM churn;
SELECT count(*) AS not_eight FROM churn WHERE cnt <> 8;
