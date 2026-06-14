use std::str::FromStr;
use std::time::Duration;

use http_utils::error::ApiError;
use reqwest::Method;
use serde::{Deserialize, Serialize};

use pageserver_api::models::{ShardImportProgress, ShardImportStatus};
use tokio_util::sync::CancellationToken;
use utils::id::{TenantId, TimelineId};
use utils::sync::gate::Gate;

use crate::{persistence::TimelineImportPersistence, service::Config};

#[derive(Deserialize, Serialize, PartialEq, Eq)]
pub(crate) enum TimelineImportState {
    Importing,
    Idle,
}

/// yb-mode: a tenant has exactly one shard, so the per-shard import status
/// map collapses to a single status.
#[derive(Serialize, Deserialize, Clone, Debug)]
pub(crate) struct ShardImportStatuses(pub(crate) ShardImportStatus);

impl ShardImportStatuses {
    pub(crate) fn new() -> Self {
        ShardImportStatuses(ShardImportStatus::InProgress(None::<ShardImportProgress>))
    }
}

#[derive(Debug)]
pub(crate) struct TimelineImport {
    pub(crate) tenant_id: TenantId,
    pub(crate) timeline_id: TimelineId,
    pub(crate) shard_statuses: ShardImportStatuses,
}

pub(crate) enum TimelineImportUpdateFollowUp {
    Persist,
    None,
}

#[derive(thiserror::Error, Debug)]
pub(crate) enum TimelineImportFinalizeError {
    #[error("Shut down interrupted import finalize")]
    ShuttingDown,
    #[error("Import finalization was cancelled")]
    Cancelled,
}

pub(crate) enum TimelineImportUpdateError {
    ImportNotFound {
        tenant_id: TenantId,
        timeline_id: TimelineId,
    },
    UnexpectedUpdate,
}

impl From<TimelineImportUpdateError> for ApiError {
    fn from(err: TimelineImportUpdateError) -> ApiError {
        match err {
            TimelineImportUpdateError::ImportNotFound {
                tenant_id,
                timeline_id,
            } => ApiError::NotFound(
                anyhow::anyhow!("Import for {tenant_id}/{timeline_id} not found").into(),
            ),
            TimelineImportUpdateError::UnexpectedUpdate => {
                ApiError::InternalServerError(anyhow::anyhow!("Update request is unexpected"))
            }
        }
    }
}

impl TimelineImport {
    pub(crate) fn from_persistent(persistent: TimelineImportPersistence) -> anyhow::Result<Self> {
        let tenant_id = TenantId::from_str(persistent.tenant_id.as_str())?;
        let timeline_id = TimelineId::from_str(persistent.timeline_id.as_str())?;
        let shard_statuses = serde_json::from_value(persistent.shard_statuses)?;

        Ok(TimelineImport {
            tenant_id,
            timeline_id,
            shard_statuses,
        })
    }

    pub(crate) fn to_persistent(&self) -> TimelineImportPersistence {
        TimelineImportPersistence {
            tenant_id: self.tenant_id.to_string(),
            timeline_id: self.timeline_id.to_string(),
            shard_statuses: serde_json::to_value(self.shard_statuses.clone()).unwrap(),
        }
    }

    pub(crate) fn update(
        &mut self,
        status: ShardImportStatus,
    ) -> Result<TimelineImportUpdateFollowUp, TimelineImportUpdateError> {
        let crnt = &mut self.shard_statuses.0;
        if *crnt == status {
            Ok(TimelineImportUpdateFollowUp::None)
        } else if crnt.is_terminal() && *crnt != status {
            Err(TimelineImportUpdateError::UnexpectedUpdate)
        } else {
            *crnt = status;
            Ok(TimelineImportUpdateFollowUp::Persist)
        }
    }

    pub(crate) fn is_complete(&self) -> bool {
        self.shard_statuses.0.is_terminal()
    }

    pub(crate) fn completion_error(&self) -> Option<String> {
        assert!(self.is_complete());

        match &self.shard_statuses.0 {
            ShardImportStatus::Error(err) => Some(err.clone()),
            _ => None,
        }
    }
}

pub(crate) struct FinalizingImport {
    pub(crate) gate: Gate,
    pub(crate) cancel: CancellationToken,
}

pub(crate) type ImportResult = Result<(), String>;

pub(crate) struct UpcallClient {
    authorization_header: Option<String>,
    client: reqwest::Client,
    cancel: CancellationToken,
    base_url: String,
}

const IMPORT_COMPLETE_REQUEST_TIMEOUT: Duration = Duration::from_secs(10);

#[derive(Serialize, Deserialize, Debug)]
struct ImportCompleteRequest {
    tenant_id: TenantId,
    timeline_id: TimelineId,
    error: Option<String>,
}

impl UpcallClient {
    pub(crate) fn new(config: &Config, cancel: CancellationToken) -> Self {
        let authorization_header = config
            .control_plane_jwt_token
            .clone()
            .map(|jwt| format!("Bearer {jwt}"));

        let client = reqwest::ClientBuilder::new()
            .timeout(IMPORT_COMPLETE_REQUEST_TIMEOUT)
            .build()
            .expect("Failed to construct HTTP client");

        let base_url = config
            .control_plane_url
            .clone()
            .expect("must be configured");

        Self {
            authorization_header,
            client,
            cancel,
            base_url,
        }
    }

    /// Notify control plane of a completed import
    ///
    /// This method guarantees at least once delivery semantics assuming
    /// eventual cplane availability. The cplane API is idempotent.
    pub(crate) async fn notify_import_complete(
        &self,
        tenant_id: TenantId,
        timeline_id: TimelineId,
        import_result: ImportResult,
    ) -> anyhow::Result<()> {
        let endpoint = if self.base_url.ends_with('/') {
            format!("{}import_complete", self.base_url)
        } else {
            format!("{}/import_complete", self.base_url)
        };

        let request = self
            .client
            .request(Method::PUT, endpoint)
            .json(&ImportCompleteRequest {
                tenant_id,
                timeline_id,
                error: import_result.err(),
            })
            .timeout(IMPORT_COMPLETE_REQUEST_TIMEOUT);

        let request = if let Some(auth) = &self.authorization_header {
            request.header(reqwest::header::AUTHORIZATION, auth)
        } else {
            request
        };

        const RETRY_DELAY: Duration = Duration::from_secs(1);
        let mut attempt = 1;

        loop {
            if self.cancel.is_cancelled() {
                return Err(anyhow::anyhow!(
                    "Shutting down while notifying cplane of import completion"
                ));
            }

            match request.try_clone().unwrap().send().await {
                Ok(response) if response.status().is_success() => {
                    return Ok(());
                }
                Ok(response) => {
                    tracing::warn!(
                        "Import complete notification failed with status {}, attempt {}",
                        response.status(),
                        attempt
                    );
                }
                Err(e) => {
                    tracing::warn!(
                        "Import complete notification failed with error: {}, attempt {}",
                        e,
                        attempt
                    );
                }
            }

            tokio::select! {
                _ = tokio::time::sleep(RETRY_DELAY) => {}
                _ = self.cancel.cancelled() => {
                    return Err(anyhow::anyhow!("Shutting down while notifying cplane of import completion"));
                }
            }
            attempt += 1;
        }
    }
}
