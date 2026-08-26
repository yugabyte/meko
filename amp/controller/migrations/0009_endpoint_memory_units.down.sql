-- 0009 down: deliberately a no-op.
--
-- The up overwrote unitless memory quantities with the default; the values it
-- replaced are not recorded anywhere, and restoring a four-byte reservation is
-- not something a downgrade wants.

SELECT 1;
