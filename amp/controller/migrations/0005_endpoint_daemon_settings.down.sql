-- 0005 down: deliberately a no-op.
--
-- The up only adds the columns where they are missing, and `0002` is what defines
-- them — so this cannot tell whether it created them or found them. Dropping them
-- on a database where `0002` created them would destroy another migration's
-- column. `0002`'s own down drops the whole table.

SELECT 1;
