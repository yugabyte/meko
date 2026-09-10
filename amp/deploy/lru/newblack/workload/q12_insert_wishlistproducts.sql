WITH candidate AS (
    SELECT $1 AS id, $2 AS regionid, $3 AS productid, $4 AS wishlistid, $5 AS createdbyid, $6 AS creationtime
)
INSERT INTO "public"."savedlistitems" (ID,RegionID,"productid","wishlistid","createdbyid","creationtime")
SELECT c.id, c.regionid, c.productid, c.wishlistid, c.createdbyid, c.creationtime
FROM candidate c
WHERE (
    SELECT count(*)
    FROM public.savedlistitems s
    WHERE s.regionid = c.regionid
      AND s.wishlistid = c.wishlistid
) < 30;
