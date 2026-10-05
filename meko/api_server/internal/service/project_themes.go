package service

// Project themes: the fixed folder layouts a user picks from when creating a
// project.
//
// These are code constants rather than a table on purpose. A theme is STAMPED
// at creation — CreateProject copies Folders into meko_system.project_folders
// and nothing reads the theme again — so a table would need CRUD with no
// reader. The project row keeps theme_key/theme_version for provenance only;
// there is no drift detection and no "upgrade to a newer theme".
//
// The UI gets this list from the API rather than hardcoding its own copy, so
// this stays the single source of truth for theme names and folder layouts.

// ProjectTheme is one selectable layout.
//
// Folders must never be nil: it is a required array on the wire, and a nil
// slice marshals to `null`, which the picker cannot map over. A theme with no
// layout carries an EMPTY slice, which is also the signal the UI reads to ask
// the user to name their first folder.
type ProjectTheme struct {
	Key         string   `json:"key"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Version     string   `json:"version"`
	Folders     []string `json:"folders"`
}

// Theme keys. Exported so handlers and tests name a theme without a literal.
const (
	ProjectThemeADLC    = "adlc"
	ProjectThemeSupport = "support"
	ProjectThemeCustom  = "custom"
)

// DefaultProjectThemeKey is used when a caller creates a project without
// naming a theme — notably the implicit project an agent's datapack_create
// lands in when it passes no path.
const DefaultProjectThemeKey = ProjectThemeCustom

// projectThemes is ordered: the UI renders the picker in this order, so the
// most-used layout comes first and Custom is the deliberate opt-out at the end.
var projectThemes = []ProjectTheme{
	{
		Key:   ProjectThemeADLC,
		Label: "Software delivery",
		Description: "Standards at the top, one datapack per feature underneath, " +
			"with lessons and test results collected alongside.",
		Version: "1.0",
		Folders: []string{"coding_standards", "features", "integration_tests"},
	},
	{
		Key:         ProjectThemeSupport,
		Label:       "Customer support",
		Description: "Product knowledge and playbooks the whole team reads, with one datapack per escalation.",
		Version:     "1.0",
		Folders:     []string{"product_knowledge", "playbooks", "escalations"},
	},
	{
		Key:   ProjectThemeCustom,
		Label: "Custom",
		Description: "Name your own first folder. Pick this when none of the layouts fit; " +
			"more folders can be added as you go.",
		Version: "1.0",
		// Deliberately empty: this theme has no layout to offer, so the caller
		// names the first folder (CreateProjectRequest.Folders). An empty list
		// is also what tells the UI to ask for one. Creating a project from it
		// without supplying a folder is refused, because a project with no
		// folders is a dead end — a datapack has to be created inside one.
		Folders: []string{},
	},
}

// ProjectThemes returns the selectable themes in display order. The slice is
// copied so a caller can't mutate the registry.
func ProjectThemes() []ProjectTheme {
	out := make([]ProjectTheme, len(projectThemes))
	copy(out, projectThemes)
	return out
}

// ProjectThemeByKey looks up a theme. The bool is false for an unknown key,
// which callers should treat as a client error rather than falling back to a
// default — silently substituting a layout would give the user a project that
// isn't the one they picked.
func ProjectThemeByKey(key string) (ProjectTheme, bool) {
	for _, t := range projectThemes {
		if t.Key == key {
			return t, true
		}
	}
	return ProjectTheme{}, false
}
