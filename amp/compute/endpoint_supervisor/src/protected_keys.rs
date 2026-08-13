//! The override deny-list, in its own module so `dump-config-keys` can include it
//! by path without dragging in the renderer. That tool ships the list to the
//! controller's API, which refuses the same keys, so it must have one definition.

/// Keys an override may never touch: the pod's identity and wiring, not tuning.
/// Overriding `branch_id` would point the endpoint at another branch's data;
/// overriding a listen address or the YB connection would unbind it from the rest
/// of the pod. Tuning knobs are open by design — this is the whole deny-list.
///
/// Matched against the FIRST SEGMENT of a dotted path, so `branch_id.anything`
/// is refused too. Every entry is a top-level key today; nesting one of these
/// would need this comparison revisited.
pub const PROTECTED_KEYS: &[&str] = &[
    "branch_id",
    "pg_distrib_dir",
    "listen_pg_addr",
    "listen_http_addr",
    "listen_grpc_addr",
    "listen_pg",
    "listen_http",
    "safekeeper_pg_addr",
    "control_plane_api",
    "yb_storage_conn",
];
