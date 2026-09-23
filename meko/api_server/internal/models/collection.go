package models

import "time"

// Collection is a user-owned container for datapacks, arranged in folders.
// Backed by meko_system.collections. Identified by collection_id;
// (user_id, collection_name) is also unique.
//
// ThemeKey/ThemeVersion are stamped at creation — the theme's folders are
// copied into meko_system.collection_folders and the theme is never read again,
// so a collection cannot drift out of sync with one.
type Collection struct {
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	ID           string    `json:"collection_id"`
	UserID       string    `json:"user_id"`
	Name         string    `json:"collection_name"`
	ThemeKey     string    `json:"theme_key"`
	ThemeVersion string    `json:"theme_version"`
	Description  string    `json:"description,omitempty"`
	// OwnerName is filled for shared collections so the recipient can see whose
	// collection they are looking at. Empty on the caller's own collections.
	OwnerName string `json:"owner_name,omitempty"`
	// FolderNames lets a collection card show the collection's shape without a
	// detail call per card. Carried on the list response because ListWithCounts
	// already reads every folder to compute FolderCount.
	//
	// On a SHARED collection this is scoped: only folders holding a datapack the
	// caller can reach appear, so sharing one datapack does not expose the rest
	// of the owner's structure.
	FolderNames []string `json:"folder_names,omitempty"`
	// FolderCount and DatapackCount are rollups, not columns. Populated by the
	// handler from one indexed count query per user (never per collection), so
	// listing N collections stays two queries rather than 2N.
	FolderCount   int `json:"folder_count"`
	DatapackCount int `json:"datapack_count"`
}

// CollectionFolder is a folder inside a collection. Backed by
// meko_system.collection_folders.
//
// Folders do NOT nest: a collection holds folders, a folder holds datapacks. That
// is why there is no parent or depth here, and why a path is always exactly
// "collection/folder".
type CollectionFolder struct {
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	ID           string    `json:"folder_id"`
	CollectionID string    `json:"collection_id"`
	Name         string    `json:"folder_name"`
	// DatapackCount is how many datapacks are filed in this folder. On a shared
	// collection it counts only the ones the caller can see.
	DatapackCount int `json:"datapack_count"`
}

// DatapackPlacement records where a datapack sits. Backed by
// meko_system.datapack_placements, one row per datapack.
//
// There is no UserID: a datapack has a single home, in a collection owned by its
// owner. Sharing shows the recipient that same collection read-only rather than
// giving them a placement of their own.
type DatapackPlacement struct {
	PlacedAt     time.Time `json:"placed_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	DatapackID   string    `json:"datapack_id"`
	CollectionID string    `json:"collection_id"`
	FolderID     string    `json:"folder_id"`
}

// FolderDatapackCount is one row of the per-folder rollup that backs the
// Collections page counts.
type FolderDatapackCount struct {
	CollectionID string `json:"collection_id"`
	FolderID     string `json:"folder_id"`
	Count        int    `json:"count"`
}

// DatapackPath is where one datapack sits, with the names already resolved.
// For a shared datapack that is the OWNER's collection, since there is only one
// home per datapack. Returned as a map keyed by datapack_id so the Datapacks
// page can label every row from a single response.
//
// Path is assembled server-side ("adlc/features") so there is one
// implementation of the separator rather than one per client. A datapack with
// no entry in the response is unfiled.
type DatapackPath struct {
	DatapackID     string `json:"datapack_id"`
	CollectionID   string `json:"collection_id"`
	CollectionName string `json:"collection_name"`
	FolderID       string `json:"folder_id"`
	// FolderName is the folder the datapack sits in ("features").
	FolderName string `json:"folder_name"`
	// Path is the full label, always collection/folder ("adlc/features").
	Path string `json:"path"`
}
