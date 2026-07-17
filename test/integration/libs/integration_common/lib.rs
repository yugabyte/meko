//! Shared helpers for the integration test suites (`compute` and `control_plane`).
//!
//! Keep this to genuinely common, suite-agnostic code. Today that's the
//! typed `tokio-postgres` assertion helpers in [`db`] — both suites open
//! connections to a compute and assert on query results. Suite-specific
//! machinery (the storage cluster fixture, the amp REST client) stays in
//! its own crate.

pub mod db;
