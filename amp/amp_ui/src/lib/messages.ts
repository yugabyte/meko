// Centralized user-facing copy for amp_ui — toasts, dialog text, hints, and
// empty states. Keeping strings here (instead of inline in components) keeps
// wording consistent and reviewable in one place; dynamic strings are exported
// as small functions.
//
// Scope so far: the Data branching (compute / branch / cluster) surfaces
// (MultitenancyPage). Move other pages' copy into this file as they're touched.

export const dataBranchingMessages = {
  // Empty / placeholder states
  selectPoolAndCluster: "Select a pool and cluster to view its branch tree.",
  noBranches: "No branches found for this cluster.",
  noCompute: "No compute instances yet.",

  // Compute provisioning
  provisionSuccess: (role: string) => `Compute provisioned (${role})`,
  provisionError: "Failed to provision compute",
  readWriteAlreadyExists:
    "This branch already has a read-write compute — additional compute must be read-only.",

  // Resize
  resizeCompleted: (size: string) => `Resized in place to ${size}`,
  resizePending: (size: string) => `Resize to ${size} accepted — applying shortly`,
  resizeInfeasible: (size: string) =>
    `Saved ${size}, but the node can't resize in place now; it applies on the next redeploy`,
  resizeUnsupported: (size: string) => `Resized to ${size} (in-place n/a on this runtime)`,
  resizeError: "Failed to resize compute",

  // Settings
  settingsTitle: "Settings",
  settingsBranchDescription:
    "Postgres settings applied to every compute on this branch. An endpoint's own settings win on a key both define.",
  settingsEndpointDescription:
    "Settings for this compute alone. Postgres settings here win over the branch's.",
  settingsRestartWarning:
    "Changing Postgres settings restarts this compute and drops its active connections. Only the services whose config actually changes are restarted.",
  settingsRestartWarningBranch:
    "Changing Postgres settings restarts the compute on every endpoint of this branch and drops their active connections. Only the services whose config actually changes are restarted.",
  settingsSaved: "Settings saved",
  settingsSavedRestarted: (services: string) => `Settings saved — restarted ${services}`,
  settingsSavedNoRestart: "Settings saved — nothing needed restarting",
  settingsSavedRestartedAndUnreachable: (services: string, unreachable: number) =>
    `Settings saved — restarted ${services}; ${unreachable} compute${unreachable === 1 ? "" : "s"} couldn't be reached and will apply them on next start`,
  settingsSavedUnreachable:
    "Settings saved, but the compute couldn't be reached — they apply when it next starts",
  settingsError: "Failed to save settings",
  settingsNothingSet: "Nothing set.",
  settingsAddRow: "Add setting",
  settingsBlankValue: (label: string, key: string) =>
    `Give ${key} a value under ${label}, or remove the row — a setting with no value stops the compute from starting.`,
  settingsDuplicateKey: (label: string, key: string) =>
    `${key} appears twice under ${label}. Edit the existing row instead of adding a second one.`,
  settingsInvalidValue: (label: string, key: string) =>
    `${key} under ${label} needs a number.`,

  // Unprovision (delete compute)
  unprovisionTitle: (role: string) => `Unprovision ${role} compute?`,
  unprovisionConfirmReadWrite:
    "This removes the branch's read-write compute — the branch won't be writable until you provision read-write compute again. The branch and its data are untouched; any active connections are dropped.",
  unprovisionConfirmReadOnly:
    "This removes this read-only compute instance. The branch and its data are untouched — you can provision compute again later. Any active connections to this endpoint will be dropped.",
  unprovisionSuccess: (role: string) => `Unprovisioned ${role} compute`,
  unprovisionError: "Failed to unprovision compute",

  // Branch creation
  branchCreated: (name: string) => `Branch "${name}" created`,
  branchCreateError: "Failed to create branch",

  // Delete branch (offered on forks only; the source branch goes with the cluster)
  deleteBranchTitle: (name: string) => `Delete branch "${name}"?`,
  deleteBranchConfirm:
    "This retires the branch's compute and permanently drops the branch and its data. This can't be undone.",
  deleteBranchSuccess: (name: string) => `Branch "${name}" deleted`,
  deleteBranchError: "Failed to delete branch",
  // 409: the branch is a fork point. Actionable — the user is looking at the tree.
  deleteBranchHasChildren:
    "This branch has sub-branches forked from it. Delete those first, then delete this branch.",

  // Delete cluster (cascades through every branch + all compute — type-to-confirm)
  deleteClusterTitle: (name: string) => `Delete cluster "${name}"?`,
  deleteClusterConfirm: (name: string) =>
    `This permanently deletes "${name}" and everything in it — every branch, all compute, and all data. This cascade can't be undone.`,
  deleteClusterConfirmPrompt: (name: string) =>
    `Type the cluster name (${name}) to confirm.`,
  deleteClusterSuccess: (name: string) => `Cluster "${name}" deleted`,
  deleteClusterError: "Failed to delete cluster",

  // Misc
  clipboardError: "Couldn't copy to clipboard",
} as const;
