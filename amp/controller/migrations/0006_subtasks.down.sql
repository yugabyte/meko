-- Reverse 0006.
--
-- A real drop: this migration is the table's definition — no
-- earlier migration creates it — so dropping it cannot destroy another
-- migration's work, and the index goes with the table. Any in-flight roll's
-- children go with it, which is what a downgrade to a controller that cannot run
-- them means.

DROP TABLE IF EXISTS subtasks;
