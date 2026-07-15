-- Ported from Neon's test_fsm_truncate (old_neon_test_runner/regress/test_fsm_truncate.py).
-- Regression guard: truncating a relation must also truncate its FSM and VM
-- forks, with no page-reconstruction error on the yb-light storage path. The
-- e2e corruption-marker + correctness-counter gates are the real assertion; the
-- SELECTs below just confirm the relations stay usable after truncate.

CREATE TABLE fsm_trunc_a(key int);
CREATE TABLE fsm_trunc_b(key int);

-- Grow the relations and materialize their FSM/VM forks: bulk insert, delete
-- half (frees space -> FSM), then VACUUM (FREEZE marks all-visible -> VM).
INSERT INTO fsm_trunc_a SELECT generate_series(1, 100000);
INSERT INTO fsm_trunc_b SELECT generate_series(1, 100000);
DELETE FROM fsm_trunc_a WHERE key % 2 = 0;
VACUUM (FREEZE) fsm_trunc_a;
VACUUM fsm_trunc_b;

-- The behaviour under test: TRUNCATE must truncate the FSM/VM forks too.
TRUNCATE TABLE fsm_trunc_a;
TRUNCATE TABLE fsm_trunc_b;

-- Reuse after truncate: re-extend and read back (exercises fork re-creation).
INSERT INTO fsm_trunc_a SELECT generate_series(1, 1000);
SELECT count(*), sum(key) FROM fsm_trunc_a;
SELECT count(*) FROM fsm_trunc_b;

DROP TABLE fsm_trunc_a;
DROP TABLE fsm_trunc_b;
