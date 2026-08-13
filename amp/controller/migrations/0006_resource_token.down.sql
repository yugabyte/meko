-- 0006 down: drop the resource-token (lease) store.
--
-- The epoch-micros helpers go too: nothing outside this migration uses them.

DROP FUNCTION IF EXISTS amp_registry.token_holder(text, bigint);
DROP FUNCTION IF EXISTS amp_registry.token_release(text, text);
DROP FUNCTION IF EXISTS amp_registry.token_acquire(text, text, bigint, bigint);
DROP TABLE    IF EXISTS amp_registry.resource_token;
DROP FUNCTION IF EXISTS amp_registry.us_to_ms(bigint);
DROP FUNCTION IF EXISTS amp_registry.us_add_ms(bigint, bigint);
DROP FUNCTION IF EXISTS amp_registry.now_us();
