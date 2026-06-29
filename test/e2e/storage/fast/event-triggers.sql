-- Ported from test_runner/sql_regress/sql/neon-event-triggers.sql. Three
-- adaptations for the e2e golden runner (vs the old pg_regress location):
--   * `\set ON_ERROR_STOP 0` — this test has *intentional* errors (the
--     permission-denied checks); the runner otherwise runs with
--     ON_ERROR_STOP=1, which would abort on the first one.
--   * `neon.event_triggers` -> `yb_amp.event_triggers` (renamed GUC).
--   * the original `\c regression` reconnect -> `\c postgres` (the e2e
--     compute's default database; there is no `regression` db here).
-- The final block drops everything this test creates: it runs against the
-- shared e2e cluster, and a leftover `ddl_command_end` trigger in `postgres`
-- would fire on — and pollute — every subsequent test.
\set ON_ERROR_STOP 0

create or replace function admin_proc()
    returns event_trigger
    language plpgsql as
$$
begin
    raise notice 'admin event trigger is executed for %', current_user;
end;
$$;

create role neon_superuser;
create role neon_admin login inherit createrole createdb in role neon_superuser;
grant create on schema public to neon_admin;
create database neondb with owner neon_admin;
grant all privileges on database neondb to neon_superuser;

create role neon_user;
grant create on schema public to neon_user;

create event trigger on_ddl1 on ddl_command_end
execute procedure admin_proc();

set role neon_user;

-- check that non-privileged user can not change yb_amp.event_triggers
set yb_amp.event_triggers to false;

-- Non-privileged neon user should not be able to create event trigers
create event trigger on_ddl2 on ddl_command_end
execute procedure admin_proc();

set role neon_admin;

-- neon_superuser should be able to create event trigers
create or replace function neon_proc()
    returns event_trigger
    language plpgsql as
$$
begin
    raise notice 'neon event trigger is executed for %', current_user;
end;
$$;

create event trigger on_ddl2 on ddl_command_end
execute procedure neon_proc();

\c neondb neon_admin

create or replace function neondb_proc()
    returns event_trigger
    language plpgsql as
$$
begin
    raise notice 'neondb event trigger is executed for %', current_user;
end;
$$;

create or replace function neondb_secdef_proc()
    returns event_trigger
    language plpgsql
    SECURITY DEFINER
as
$$
begin
    raise notice 'neondb secdef event trigger is executed for %', current_user;
end;
$$;

-- neon_admin (neon_superuser member) should be able to create event triggers
create event trigger on_ddl3 on ddl_command_end
execute procedure neondb_proc();

create event trigger on_ddl4 on ddl_command_end
execute procedure neondb_secdef_proc();

-- Check that event trigger is fired for neon_admin
create table t1(x integer);

-- Check that event trigger can be skipped
set yb_amp.event_triggers to false;
create table t2(x integer);

\c postgres cloud_admin

-- Check that event triggers are not fired for superuser
create table t3(x integer);

\c neondb cloud_admin

-- Check that user-defined event triggers are not fired for superuser
create table t4(x integer);

\c neondb neon_admin

-- Check that neon_admin can drop event triggers
drop event trigger on_ddl3;
drop event trigger on_ddl4;

-- Clean up everything this test created so it doesn't leak into the shared
-- e2e cluster (drop triggers before their functions; drop neondb before the
-- roles that own it; DROP OWNED BY clears the schema-public grants that
-- would otherwise block DROP ROLE).
\c postgres cloud_admin
drop event trigger if exists on_ddl1;
drop event trigger if exists on_ddl2;
drop function if exists neon_proc();
drop function if exists admin_proc();
drop database if exists neondb;
drop owned by neon_admin, neon_user, neon_superuser cascade;
drop role if exists neon_admin;
drop role if exists neon_user;
drop role if exists neon_superuser;
