// @generated automatically by Diesel CLI.

pub mod sql_types {
    #[derive(diesel::query_builder::QueryId, diesel::sql_types::SqlType)]
    #[diesel(postgres_type(name = "pg_lsn", schema = "pg_catalog"))]
    pub struct PgLsn;
}

diesel::table! {
    endpoints (endpoint_id) {
        endpoint_id -> Varchar,
        tenant_id -> Varchar,
        timeline_id -> Varchar,
        spec -> Jsonb,
    }
}

diesel::table! {
    metadata_health (tenant_id) {
        tenant_id -> Varchar,
        healthy -> Bool,
        last_scrubbed_at -> Timestamptz,
    }
}

diesel::table! {
    nodes (node_id) {
        node_id -> Int8,
        scheduling_policy -> Varchar,
        listen_http_addr -> Varchar,
        listen_http_port -> Int4,
        listen_pg_addr -> Varchar,
        listen_pg_port -> Int4,
        listen_https_port -> Nullable<Int4>,
        lifecycle -> Varchar,
        listen_grpc_addr -> Nullable<Varchar>,
        listen_grpc_port -> Nullable<Int4>,
    }
}

diesel::table! {
    safekeepers (id) {
        id -> Int8,
        region_id -> Text,
        version -> Int8,
        host -> Text,
        port -> Int4,
        http_port -> Int4,
        availability_zone_id -> Text,
        scheduling_policy -> Varchar,
        https_port -> Nullable<Int4>,
    }
}

diesel::table! {
    tenants (tenant_id) {
        tenant_id -> Varchar,
        generation -> Nullable<Int4>,
        generation_pageserver -> Nullable<Int8>,
        placement_policy -> Varchar,
        config -> Jsonb,
        scheduling_policy -> Varchar,
    }
}

diesel::table! {
    timeline_imports (tenant_id, timeline_id) {
        tenant_id -> Varchar,
        timeline_id -> Varchar,
        shard_statuses -> Jsonb,
    }
}

diesel::table! {
    use diesel::sql_types::*;
    use super::sql_types::PgLsn;

    timelines (tenant_id, timeline_id) {
        tenant_id -> Varchar,
        timeline_id -> Varchar,
        start_lsn -> PgLsn,
        generation -> Int4,
        sk_set -> Array<Nullable<Int8>>,
        deleted_at -> Nullable<Timestamptz>,
        branch_name -> Nullable<Varchar>,
    }
}

diesel::table! {
    use diesel::sql_types::*;
    use super::sql_types::PgLsn;

    pending_timeline_inits (tenant_id, timeline_id) {
        tenant_id -> Varchar,
        timeline_id -> Varchar,
        branch_name -> Varchar,
        pg_version -> Int4,
        ancestor_timeline_id -> Nullable<Varchar>,
        start_lsn -> PgLsn,
    }
}

diesel::allow_tables_to_appear_in_same_query!(
    endpoints,
    metadata_health,
    nodes,
    pending_timeline_inits,
    safekeepers,
    tenants,
    timeline_imports,
    timelines,
);
