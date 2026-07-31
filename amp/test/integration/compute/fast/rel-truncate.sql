--
-- Test that when a relation is truncated by VACUUM, the next smgrnblocks()
-- query to get the relation's size returns the new size.
-- (This isn't related to the TRUNCATE command, which works differently,
-- by creating a new relation file)
--
-- autovacuum off (as in gist.sql / read-validation.sql): this test hinges on
-- VACUUM actually *truncating* the relation, and `lazy_truncate_heap` abandons
-- the truncation when another backend waits on the ACCESS EXCLUSIVE lock it
-- needs. An autovacuum worker landing on this table mid-test would make the
-- explicit VACUUMs below silently skip truncating — which surfaces as the
-- assertions here failing for a reason that has nothing to do with storage.
CREATE TABLE truncatetest (i int) WITH (autovacuum_enabled = off);
INSERT INTO truncatetest SELECT g FROM generate_series(1, 10000) g;

-- Remove all the rows, and run VACUUM to remove the dead tuples and
-- truncate the physical relation to 0 blocks.
DELETE FROM truncatetest;
VACUUM truncatetest;

-- Check that a SeqScan sees correct relation size (which is now 0)
SELECT * FROM truncatetest;

DROP TABLE truncatetest;


--
-- Test that the FSM is truncated along with the table.
--

-- Create a test table and delete and vacuum away most of the rows.
-- This leaves the FSM full of pages with plenty of space
create table tt(i int) with (autovacuum_enabled = off);
insert into tt select g from generate_series(1, 100000) g;
delete from tt where i%100 != 0 and i > 10000;
vacuum freeze tt;

-- Delete the rest of the rows, and vacuum again. This truncates the
-- heap to 0 blocks, and should also truncate the FSM.
delete from tt;
vacuum tt;

-- This can be used to look at the FSM directly, if the 'pg_freespace' contrib module
-- is installed
--SELECT blkno, avail from generate_series(1, 450) blkno, pg_freespace('tt'::regclass, blkno) AS avail;

-- Insert a row again. It should go on block #0. If the FSM was not truncated,
-- the insertion would find a higher-numbered block in the FSM and use that instead.
insert into tt values (0);
select ctid, * from tt;

drop table tt;
