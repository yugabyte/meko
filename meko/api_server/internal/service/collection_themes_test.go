package service

import "testing"

// Exactly one theme may seed no folders: Blank, which asks the caller to name
// the first one. Any OTHER empty theme is a bug twice over — it produces a
// collection no datapack can be created in, and an empty list is also the signal
// the UI uses to prompt for a folder name, so the prompt would appear for a
// layout that was supposed to supply its own.
func TestCollectionThemes_OnlyBlankSeedsNoFolders(t *testing.T) {
	for _, theme := range CollectionThemes() {
		if len(theme.Folders) == 0 && theme.Key != CollectionThemeCustom {
			t.Errorf("theme %q seeds no folders; only %q may do that", theme.Key, CollectionThemeCustom)
		}
	}
}

// Folder names within a theme must be unique: uq_collection_folders_root_name
// would reject the duplicate at stamp time and fail the whole create in a
// transaction, so a typo here breaks collection creation outright.
func TestCollectionThemes_FolderNamesUniqueWithinTheme(t *testing.T) {
	for _, theme := range CollectionThemes() {
		seen := make(map[string]bool, len(theme.Folders))
		for _, folder := range theme.Folders {
			if seen[folder] {
				t.Errorf("theme %q repeats folder %q; the unique index would abort the create", theme.Key, folder)
			}
			seen[folder] = true
		}
	}
}

// Theme keys are persisted on the collection row, so a duplicate key would make
// the stamped provenance ambiguous.
func TestCollectionThemes_KeysUnique(t *testing.T) {
	seen := map[string]bool{}
	for _, theme := range CollectionThemes() {
		if seen[theme.Key] {
			t.Errorf("duplicate theme key %q", theme.Key)
		}
		seen[theme.Key] = true
	}
}

// The default must resolve — Create falls back to it when no theme is named
// (an agent creating an implicit collection), so an unresolvable default would
// fail that path at runtime rather than here.
func TestCollectionThemes_DefaultResolves(t *testing.T) {
	if _, found := CollectionThemeByKey(DefaultCollectionThemeKey); !found {
		t.Fatalf("DefaultCollectionThemeKey %q is not a registered theme", DefaultCollectionThemeKey)
	}
}

func TestCollectionThemeByKey_UnknownIsNotFound(t *testing.T) {
	if _, found := CollectionThemeByKey("no-such-theme"); found {
		t.Error("expected an unknown key to report not-found rather than fall back to a default")
	}
}

// CollectionThemes hands out a copy: a caller mutating the result must not
// reshape the registry for every later request in the process.
func TestCollectionThemes_ReturnsCopy(t *testing.T) {
	first := CollectionThemes()
	if len(first) == 0 {
		t.Fatal("no themes registered")
	}
	original := first[0].Key
	first[0].Key = "mutated"

	if CollectionThemes()[0].Key != original {
		t.Error("mutating the returned slice changed the registry")
	}
}
