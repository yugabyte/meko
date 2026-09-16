-- Reverse 0019.
--
-- A real drop: this migration is the table's definition, so dropping it destroys
-- nothing another migration made. The registrations go with it, which is what
-- downgrading to a controller that cannot read them means — any branch that had
-- opted into a registered extension keeps the NAME in
-- amp_registry.branches.extensions, and the catalog then drops it as unknown at
-- resolve time (Resolve skips names it does not know), so those endpoints
-- redeploy without the mount rather than failing.

DROP TABLE IF EXISTS amp_registry.extensions;
