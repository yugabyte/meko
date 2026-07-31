DO $$
DECLARE
    bypassrls boolean;
BEGIN
    SELECT rolbypassrls INTO bypassrls FROM pg_catalog.pg_roles WHERE rolname = 'yb_amp_superuser';
    IF NOT bypassrls THEN
        RAISE EXCEPTION 'yb_amp_superuser cannot bypass RLS';
    END IF;
END $$;
