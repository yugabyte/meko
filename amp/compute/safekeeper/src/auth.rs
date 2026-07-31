use utils::auth::{AuthError, Claims, Scope};
use utils::id::BranchId;

/// If branch_id is provided, allow if token (claims) is for this branch or
/// whole safekeeper scope (SafekeeperData). Else, allow only if token is
/// SafekeeperData.
pub fn check_permission(claims: &Claims, branch_id: Option<BranchId>) -> Result<(), AuthError> {
    match (&claims.scope, branch_id) {
        (Scope::Tenant, None) => Err(AuthError(
            "Attempt to access management api with tenant scope. Permission denied".into(),
        )),
        (Scope::Tenant, Some(branch_id)) => {
            if claims.branch_id.unwrap() != branch_id {
                return Err(AuthError("Branch id mismatch. Permission denied".into()));
            }
            Ok(())
        }
        (
            Scope::Admin
            | Scope::PageServerApi
            | Scope::GenerationsApi
            | Scope::Infra
            | Scope::Scrubber
            | Scope::ControllerPeer
            | Scope::TenantEndpoint,
            _,
        ) => Err(AuthError(
            format!(
                "JWT scope '{:?}' is ineligible for Safekeeper auth",
                claims.scope
            )
            .into(),
        )),
        (Scope::SafekeeperData, _) => Ok(()),
    }
}
