//! Which Unix user each child runs as.
//!
//! With `STORAGE_UID`/`STORAGE_GID` and `COMPUTE_UID`/`COMPUTE_GID` set, the
//! supervisor (root) starts the pageserver and safekeeper as the storage user and
//! `compute_ctl` as the compute user. The storage user owns the YugabyteDB client
//! key; the compute user cannot read it, so a compromised Postgres has no
//! credential for the foundation. Unset, every child inherits this process's
//! identity, which is what `yb_amp_local` and every pre-split pod run.

use anyhow::{Result, bail};

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct RunAs {
    pub uid: u32,
    pub gid: u32,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Identities {
    pub storage: RunAs,
    pub compute: RunAs,
}

const VARS: [&str; 4] = ["STORAGE_UID", "STORAGE_GID", "COMPUTE_UID", "COMPUTE_GID"];

impl Identities {
    pub fn from_env() -> Result<Option<Self>> {
        Self::parse(crate::env_opt, unsafe { libc::geteuid() })
    }

    /// Refuses a partial or unsafe configuration rather than running with less
    /// isolation than the operator asked for: a split that silently falls back to
    /// one user leaves the key readable while the deployment claims otherwise.
    fn parse(get: impl Fn(&str) -> Option<String>, euid: u32) -> Result<Option<Self>> {
        let vals: Vec<Option<String>> = VARS.iter().map(|k| get(k)).collect();
        if vals.iter().all(Option::is_none) {
            return Ok(None);
        }
        let mut ids = [0u32; 4];
        for ((name, val), id) in VARS.iter().zip(&vals).zip(ids.iter_mut()) {
            let Some(v) = val else {
                bail!(
                    "{name} is unset; the user split needs all of {}",
                    VARS.join(", ")
                );
            };
            *id = v
                .parse()
                .map_err(|_| anyhow::anyhow!("{name}={v:?} is not a numeric id"))?;
            if *id == 0 {
                bail!("{name} is 0; a child must not run as root");
            }
        }
        let [su, sg, cu, cg] = ids;
        if su == cu || sg == cg {
            bail!(
                "storage and compute must differ in both uid and gid (storage {su}:{sg}, compute {cu}:{cg})"
            );
        }
        if euid != 0 {
            bail!("the user split needs the supervisor to run as root (euid {euid})");
        }
        Ok(Some(Self {
            storage: RunAs { uid: su, gid: sg },
            compute: RunAs { uid: cu, gid: cg },
        }))
    }
}

/// Runs in the child between `fork` and `exec`, after std has applied `setgroups`,
/// `setgid` and `setuid`. Only async-signal-safe calls are allowed here.
///
/// std ignores an `EPERM` from its `setgroups`, so the supplementary groups are
/// checked rather than assumed: a child that kept the pod's `fsGroup` could read
/// the client key. no-new-privileges stops the child regaining privileges through
/// a setuid or file-capability binary in the image.
pub fn harden_child() -> std::io::Result<()> {
    // SAFETY: getgroups with a zero size only returns the count.
    if unsafe { libc::getgroups(0, std::ptr::null_mut()) } != 0 {
        return Err(std::io::Error::from_raw_os_error(libc::EPERM));
    }
    #[cfg(target_os = "linux")]
    {
        // SAFETY: prctl(PR_SET_NO_NEW_PRIVS) takes no pointers.
        if unsafe { libc::prctl(libc::PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0) } != 0 {
            return Err(std::io::Error::last_os_error());
        }
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    fn env(pairs: &[(&str, &str)]) -> impl Fn(&str) -> Option<String> {
        let pairs: Vec<(String, String)> = pairs
            .iter()
            .map(|(k, v)| (k.to_string(), v.to_string()))
            .collect();
        move |k| pairs.iter().find(|(n, _)| n == k).map(|(_, v)| v.clone())
    }

    const FULL: [(&str, &str); 4] = [
        ("STORAGE_UID", "1000"),
        ("STORAGE_GID", "1000"),
        ("COMPUTE_UID", "1001"),
        ("COMPUTE_GID", "1001"),
    ];

    #[test]
    fn unset_means_no_split() {
        assert_eq!(Identities::parse(env(&[]), 1000).unwrap(), None);
    }

    #[test]
    fn a_full_set_as_root_splits() {
        let ids = Identities::parse(env(&FULL), 0).unwrap().unwrap();
        assert_eq!(
            ids.storage,
            RunAs {
                uid: 1000,
                gid: 1000
            }
        );
        assert_eq!(
            ids.compute,
            RunAs {
                uid: 1001,
                gid: 1001
            }
        );
    }

    #[test]
    fn a_partial_set_is_refused() {
        let err = Identities::parse(env(&FULL[..3]), 0).unwrap_err();
        assert!(err.to_string().contains("COMPUTE_GID is unset"), "{err}");
    }

    #[test]
    fn root_children_are_refused() {
        let mut e = FULL;
        e[2] = ("COMPUTE_UID", "0");
        assert!(Identities::parse(env(&e), 0).is_err());
    }

    #[test]
    fn a_shared_uid_or_gid_is_refused() {
        let mut e = FULL;
        e[3] = ("COMPUTE_GID", "1000");
        assert!(Identities::parse(env(&e), 0).is_err());
    }

    #[test]
    fn a_non_root_supervisor_is_refused() {
        let err = Identities::parse(env(&FULL), 1000).unwrap_err();
        assert!(err.to_string().contains("root"), "{err}");
    }
}
