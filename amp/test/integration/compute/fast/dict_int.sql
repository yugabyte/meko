-- dict_int ships in the compute image as stock contrib.
CREATE EXTENSION dict_int;

-- The default intdict truncates to maxlen 6; REJECTLONG drops the token instead.
SELECT ts_lexize('intdict', '12345678');
ALTER TEXT SEARCH DICTIONARY intdict (MAXLEN = 4, REJECTLONG = true);
SELECT ts_lexize('intdict', '12345678');
SELECT ts_lexize('intdict', '1234');

-- Leave the shared suite database as found.
DROP EXTENSION dict_int;
