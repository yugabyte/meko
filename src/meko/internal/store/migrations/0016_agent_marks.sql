-- Context Gaps: an agent's own latest dismissal and teach (Kode's learn step: a mark with a
-- conversation), kept apart as a person's are (person_*_at), so a person's later mark doesn't take
-- them out of the usage stats. Marks made before are filled in from the ones still standing.
ALTER TABLE questions ADD COLUMN agent_dismissed_at TEXT;
ALTER TABLE questions ADD COLUMN agent_taught_at TEXT;
UPDATE questions SET agent_dismissed_at = dismissed_at WHERE dismissed_by IS NOT NULL;
UPDATE questions SET agent_taught_at = taught_at WHERE taught_by IS NOT NULL;
