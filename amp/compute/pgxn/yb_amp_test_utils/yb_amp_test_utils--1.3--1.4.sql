-- complain if script is sourced in psql, rather than via ALTER EXTENSION
\echo Use "ALTER EXTENSION yb_amp_test_utils UPDATE TO '1.4'" to load this file. \quit

CREATE FUNCTION test_generic_xlog_new_page(rel regclass, fill int)
RETURNS int8
AS 'MODULE_PATHNAME', 'test_generic_xlog_new_page'
LANGUAGE C STRICT
PARALLEL UNSAFE;
