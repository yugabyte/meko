-- First-writer-wins lock for endpoint resize.
--
-- A resize (`resize_endpoint` task) should hold its endpoint until it finishes:
-- while one is in flight, a second concurrent resize of the same endpoint must be
-- rejected (409) rather than racing it to a nondeterministic final size. This
-- partial unique index enforces exactly that — at most one non-terminal
-- `resize_endpoint` task per endpoint (`target_ref` is the endpoint id). The
-- second concurrent INSERT violates it and the store surfaces a conflict.
--
-- The lock is self-releasing: when the in-flight task reaches a terminal status
-- (`succeeded`/`failed`) it drops out of the index's predicate, so the next
-- resize is admitted. A crashed task is reclaimed via its lease and eventually
-- reaches terminal too, so the lock cannot wedge permanently.
CREATE UNIQUE INDEX task_one_active_resize_per_endpoint
    ON task (target_ref)
    WHERE kind = 'resize_endpoint' AND status IN ('pending', 'running');
