//!
//! This module provides metric definitions for the storage controller.
//!
//! All metrics are grouped in [`StorageControllerMetricGroup`]. [`StorageControllerMetrics`] holds
//! the mentioned metrics and their encoder. It's globally available via the [`METRICS_REGISTRY`]
//! constant.
//!
//! The rest of the code defines label group types and deals with converting outer types to labels.
//!
use std::sync::Mutex;

use bytes::Bytes;
use measured::label::LabelValue;
use measured::metric::histogram;
use measured::{FixedCardinalityLabel, MetricGroup};
use metrics::NeonMetrics;
use once_cell::sync::Lazy;

use crate::persistence::{DatabaseError, DatabaseOperation};
pub(crate) static METRICS_REGISTRY: Lazy<StorageControllerMetrics> =
    Lazy::new(StorageControllerMetrics::default);

pub fn preinitialize_metrics() {
    Lazy::force(&METRICS_REGISTRY);
}

pub(crate) struct StorageControllerMetrics {
    pub(crate) metrics_group: StorageControllerMetricGroup,
    encoder: Mutex<measured::text::BufferedTextEncoder>,
}

#[derive(measured::MetricGroup)]
#[metric(new())]
pub(crate) struct StorageControllerMetricGroup {
    /// Size of the in-memory map of branches
    pub(crate) storage_controller_branches: measured::Gauge,

    /// Size of the in-memory map of pageserver_nodes
    pub(crate) storage_controller_pageserver_nodes: measured::Gauge,

    /// Count of how many pageserver nodes from in-memory map have https configured
    pub(crate) storage_controller_https_pageserver_nodes: measured::Gauge,

    /// How many shard locations (secondary or attached) on each node
    pub(crate) storage_controller_node_shards: measured::GaugeVec<NodeLabelGroupSet>,

    /// How many _attached_ shard locations on each node
    pub(crate) storage_controller_node_attached_shards: measured::GaugeVec<NodeLabelGroupSet>,

    /// HTTP request status counters for handled requests
    pub(crate) storage_controller_http_request_status:
        measured::CounterVec<HttpRequestStatusLabelGroupSet>,

    /// HTTP request handler latency across all status codes
    #[metric(metadata = histogram::Thresholds::exponential_buckets(0.1, 2.0))]
    pub(crate) storage_controller_http_request_latency:
        measured::HistogramVec<HttpRequestLatencyLabelGroupSet, 5>,

    /// HTTP rate limiting latency across all tenants and endpoints
    #[metric(metadata = histogram::Thresholds::exponential_buckets(0.1, 10.0))]
    pub(crate) storage_controller_http_request_rate_limited: measured::Histogram<10>,

    /// Count of HTTP requests to the pageserver that resulted in an error,
    /// broken down by the pageserver node id, request name and method
    pub(crate) storage_controller_pageserver_request_error:
        measured::CounterVec<PageserverRequestLabelGroupSet>,

    /// Latency of HTTP requests to the pageserver, broken down by pageserver
    /// node id, request name and method. This include both successful and unsuccessful
    /// requests.
    #[metric(metadata = histogram::Thresholds::exponential_buckets(0.1, 2.0))]
    pub(crate) storage_controller_pageserver_request_latency:
        measured::HistogramVec<PageserverRequestLabelGroupSet, 5>,

    /// Count of pass-through HTTP requests to the pageserver that resulted in an error,
    /// broken down by the pageserver node id, request name and method
    pub(crate) storage_controller_passthrough_request_error:
        measured::CounterVec<PageserverRequestLabelGroupSet>,

    /// Latency of pass-through HTTP requests to the pageserver, broken down by pageserver
    /// node id, request name and method. This include both successful and unsuccessful
    /// requests.
    #[metric(metadata = histogram::Thresholds::exponential_buckets(0.1, 2.0))]
    pub(crate) storage_controller_passthrough_request_latency:
        measured::HistogramVec<PageserverRequestLabelGroupSet, 5>,

    /// Count of errors in database queries, broken down by error type and operation.
    pub(crate) storage_controller_database_query_error:
        measured::CounterVec<DatabaseQueryErrorLabelGroupSet>,

    /// Latency of database queries, broken down by operation.
    #[metric(metadata = histogram::Thresholds::exponential_buckets(0.1, 2.0))]
    pub(crate) storage_controller_database_query_latency:
        measured::HistogramVec<DatabaseQueryLatencyLabelGroupSet, 5>,

    /// Set to 1 if we detect any page server pods with pending node pool rotation annotations.
    /// Requires manual reset after oncall investigation.
    pub(crate) storage_controller_ps_node_pool_rotation_pending: measured::Gauge,

    /// Storage scrubber status.
    pub(crate) storage_controller_storage_scrub_status:
        measured::CounterVec<StorageScrubberLabelGroupSet>,

    /// Desired number of pageservers managed by the storage controller
    pub(crate) storage_controller_num_pageservers_desired: measured::Gauge,
}

impl StorageControllerMetrics {
    pub(crate) fn encode(&self, neon_metrics: &NeonMetrics) -> Bytes {
        let mut encoder = self.encoder.lock().unwrap();
        neon_metrics
            .collect_group_into(&mut *encoder)
            .unwrap_or_else(|infallible| match infallible {});
        self.metrics_group
            .collect_group_into(&mut *encoder)
            .unwrap_or_else(|infallible| match infallible {});
        encoder.finish()
    }
}

impl Default for StorageControllerMetrics {
    fn default() -> Self {
        Self {
            metrics_group: StorageControllerMetricGroup::new(),
            encoder: Mutex::new(measured::text::BufferedTextEncoder::new()),
        }
    }
}

#[derive(measured::LabelGroup, Clone)]
#[label(set = NodeLabelGroupSet)]
pub(crate) struct NodeLabelGroup<'a> {
    #[label(dynamic_with = lasso::ThreadedRodeo, default)]
    pub(crate) node_id: &'a str,
}

#[derive(measured::LabelGroup)]
#[label(set = HttpRequestStatusLabelGroupSet)]
pub(crate) struct HttpRequestStatusLabelGroup<'a> {
    #[label(dynamic_with = lasso::ThreadedRodeo, default)]
    pub(crate) path: &'a str,
    pub(crate) method: Method,
    pub(crate) status: StatusCode,
}

#[derive(measured::LabelGroup)]
#[label(set = HttpRequestLatencyLabelGroupSet)]
pub(crate) struct HttpRequestLatencyLabelGroup<'a> {
    #[label(dynamic_with = lasso::ThreadedRodeo, default)]
    pub(crate) path: &'a str,
    pub(crate) method: Method,
}

#[derive(measured::LabelGroup, Clone)]
#[label(set = PageserverRequestLabelGroupSet)]
pub(crate) struct PageserverRequestLabelGroup<'a> {
    #[label(dynamic_with = lasso::ThreadedRodeo, default)]
    pub(crate) pageserver_id: &'a str,
    #[label(dynamic_with = lasso::ThreadedRodeo, default)]
    pub(crate) path: &'a str,
    pub(crate) method: Method,
}

#[derive(measured::LabelGroup)]
#[label(set = DatabaseQueryErrorLabelGroupSet)]
pub(crate) struct DatabaseQueryErrorLabelGroup {
    pub(crate) error_type: DatabaseErrorLabel,
    pub(crate) operation: DatabaseOperation,
}

#[derive(measured::LabelGroup)]
#[label(set = DatabaseQueryLatencyLabelGroupSet)]
pub(crate) struct DatabaseQueryLatencyLabelGroup {
    pub(crate) operation: DatabaseOperation,
}

#[derive(measured::LabelGroup, Clone)]
#[label(set = StorageScrubberLabelGroupSet)]
pub(crate) struct StorageScrubberLabelGroup<'a> {
    #[label(dynamic_with = lasso::ThreadedRodeo, default)]
    pub(crate) branch_id: &'a str,
    pub(crate) outcome: StorageScrubberOutcome,
}

#[derive(FixedCardinalityLabel, Clone, Copy)]
pub(crate) enum StorageScrubberOutcome {
    PSOk,
    PSWarning,
    PSError,
    PSOrphan,
    SKOk,
    SKError,
}

#[derive(FixedCardinalityLabel, Copy, Clone)]
pub(crate) enum Method {
    Get,
    Put,
    Post,
    Delete,
    Other,
}

impl From<hyper::Method> for Method {
    fn from(value: hyper::Method) -> Self {
        if value == hyper::Method::GET {
            Method::Get
        } else if value == hyper::Method::PUT {
            Method::Put
        } else if value == hyper::Method::POST {
            Method::Post
        } else if value == hyper::Method::DELETE {
            Method::Delete
        } else {
            Method::Other
        }
    }
}

#[derive(Clone, Copy)]
pub(crate) struct StatusCode(pub(crate) hyper::http::StatusCode);

impl LabelValue for StatusCode {
    fn visit<V: measured::label::LabelVisitor>(&self, v: V) -> V::Output {
        v.write_int(self.0.as_u16() as i64)
    }
}

impl FixedCardinalityLabel for StatusCode {
    fn cardinality() -> usize {
        (100..1000).len()
    }

    fn encode(&self) -> usize {
        self.0.as_u16() as usize
    }

    fn decode(value: usize) -> Self {
        Self(hyper::http::StatusCode::from_u16(u16::try_from(value).unwrap()).unwrap())
    }
}

#[derive(FixedCardinalityLabel, Clone, Copy)]
pub(crate) enum DatabaseErrorLabel {
    Query,
    Connection,
    ConnectionPool,
    Logical,
    Migration,
}

impl DatabaseError {
    pub(crate) fn error_label(&self) -> DatabaseErrorLabel {
        match self {
            Self::Query(_) => DatabaseErrorLabel::Query,
            Self::Connection(_) => DatabaseErrorLabel::Connection,
            Self::ConnectionPool(_) => DatabaseErrorLabel::ConnectionPool,
            Self::Logical(_) => DatabaseErrorLabel::Logical,
            Self::Migration(_) => DatabaseErrorLabel::Migration,
        }
    }
}
