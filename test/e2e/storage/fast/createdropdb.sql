-- CREATE/DROP DATABASE through the pageserver: relmapper + dbdir handling.
-- VACUUM FULL pg_class first so the template's relmapper has changed, then
-- create a database from it, connect in, write + read a table, reconnect
-- out, and drop it. Ported from Neon's test_createdb / test_dropdb.
VACUUM FULL pg_class;
CREATE DATABASE e2e_createdropdb;

\c e2e_createdropdb
CREATE TABLE cddb_t (id int);
INSERT INTO cddb_t SELECT generate_series(1, 100);
SELECT count(*) FROM cddb_t;

\c postgres
DROP DATABASE e2e_createdropdb;
