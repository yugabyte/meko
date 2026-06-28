use utils::auth::{AuthError, Claims, Scope};
use utils::id::BranchId;

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
        (Scope::PageServerApi, None) => Ok(()), // access to management api for PageServerApi scope
        (Scope::PageServerApi, Some(_)) => Ok(()), // access to branch api using PageServerApi scope
        (
            Scope::Admin
            | Scope::SafekeeperData
            | Scope::GenerationsApi
            | Scope::Infra
            | Scope::Scrubber
            | Scope::ControllerPeer
            | Scope::TenantEndpoint,
            _,
        ) => Err(AuthError(
            format!(
                "JWT scope '{:?}' is ineligible for Pageserver auth",
                claims.scope
            )
            .into(),
        )),
    }
}
