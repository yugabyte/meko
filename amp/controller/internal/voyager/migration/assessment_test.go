package migration

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseAssessmentPayloadV18(t *testing.T) {
	payload := []byte(`{
		"PayloadVersion": "1.8",
		"VoyagerVersion": "1.7.2",
		"MigrationComplexity": "MEDIUM",
		"MigrationComplexityExplanation": "Found compatibility issues",
		"SourceSizeDetails": {
			"TotalDBSize": 9999,
			"TotalTableSize": 7000,
			"TotalIndexSize": 2000,
			"TotalTableRowCount": 12345
		},
		"Sizing": {
			"SizingRecommendation": {
				"ColocatedTables": ["public.small"],
				"ColocatedReasoning": "Small tables can be colocated",
				"ShardedTables": ["public.large"],
				"NumNodes": 3,
				"VCPUsPerInstance": 8,
				"MemoryPerInstance": 32,
				"OptimalSelectConnectionsPerNode": 10,
				"OptimalInsertConnectionsPerNode": 12,
				"EstimatedTimeInMinForImport": 20,
				"EstimatedTimeInMinForImportWithoutRedundantIndexes": 15
			},
			"FailureReasoning": ""
		},
		"AssessmentIssues": [{
			"Category": "unsupported_features",
			"CategoryDescription": "Unsupported features",
			"Type": "GIN_INDEXES",
			"Name": "GIN indexes",
			"Description": "Rework the index",
			"Impact": "Level-2",
			"ObjectType": "INDEX",
			"ObjectName": "public.docs_idx",
			"ObjectUsage": "used",
			"SqlStatement": "CREATE INDEX ...",
			"DocsLink": "https://example.test/gin",
			"MinimumVersionsFixedIn": {},
			"Details": {"key": "value"}
		}],
		"GeneralNotes": ["general"],
		"ColocatedShardedNotes": ["placement"],
		"SizingNotes": ["sizing"],
		"Notes": ["legacy"]
	}`)

	got, err := ParseAssessmentPayload(payload)
	if err != nil {
		t.Fatalf("ParseAssessmentPayload() error = %v", err)
	}
	if got.VoyagerVersion != "1.7.2" ||
		got.MigrationComplexity != "MEDIUM" ||
		got.MigrationComplexityExplanation != "Found compatibility issues" {
		t.Fatalf("unexpected assessment identity fields: %#v", got)
	}
	if got.SourceDatabase != (SourceDBStats{
		TotalTableSizeBytes: 7000,
		TotalIndexSizeBytes: 2000,
		TableRowCount:       12345,
	}) {
		t.Fatalf("SourceDatabase = %#v", got.SourceDatabase)
	}
	if got.SizingRecommendation.NumNodes != 3 ||
		got.SizingRecommendation.VCPUPerNode != 8 ||
		got.SizingRecommendation.MemoryPerNodeGB != 32 ||
		got.SizingRecommendation.Summary != "Small tables can be colocated" {
		t.Fatalf("SizingRecommendation = %#v", got.SizingRecommendation)
	}
	if !reflect.DeepEqual(got.SizingRecommendation.ColocatedTables, []string{"public.small"}) ||
		!reflect.DeepEqual(got.SizingRecommendation.ShardedTables, []string{"public.large"}) {
		t.Fatalf("unexpected table recommendations: %#v", got.SizingRecommendation)
	}
	if len(got.Issues) != 1 || got.Issues[0] != (AssessmentIssue{
		Category:    "unsupported_features",
		Type:        "GIN_INDEXES",
		Name:        "GIN indexes",
		Description: "Rework the index",
		Impact:      "Level-2",
		ObjectType:  "INDEX",
		ObjectName:  "public.docs_idx",
		DocsLink:    "https://example.test/gin",
	}) {
		t.Fatalf("Issues = %#v", got.Issues)
	}
	if !reflect.DeepEqual(got.Notes, []string{"general", "placement", "sizing"}) {
		t.Fatalf("Notes = %#v", got.Notes)
	}
}

func TestParseAssessmentPayloadFallsBackToLegacyNotes(t *testing.T) {
	got, err := ParseAssessmentPayload([]byte(`{
		"PayloadVersion": "1.8",
		"Notes": ["legacy note"]
	}`))
	if err != nil {
		t.Fatalf("ParseAssessmentPayload() error = %v", err)
	}
	if !reflect.DeepEqual(got.Notes, []string{"legacy note"}) {
		t.Fatalf("Notes = %#v", got.Notes)
	}
	if got.Issues == nil || got.SizingRecommendation.ColocatedTables == nil ||
		got.SizingRecommendation.ShardedTables == nil {
		t.Fatalf("slice projections must be non-nil: %#v", got)
	}
}

func TestParseAssessmentPayloadRejectsUnsupportedVersion(t *testing.T) {
	_, err := ParseAssessmentPayload([]byte(`{"PayloadVersion":"1.7"}`))
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("ParseAssessmentPayload() error = %v, want unsupported version", err)
	}
}

func TestSupportedAssessmentVersion(t *testing.T) {
	cases := map[string]bool{
		"1.8":  true,  // documented minimum
		"1.9":  true,  // newer minor is additive/backward-compatible
		"1.10": true,  // numeric compare, not string compare
		"1.7":  false, // older minor
		"2.0":  false, // major bump signals a breaking change
		"1":    false, // malformed
		"":     false, // empty
		"x.y":  false, // non-numeric
	}
	for v, want := range cases {
		if got := supportedAssessmentVersion(v); got != want {
			t.Errorf("supportedAssessmentVersion(%q) = %v, want %v", v, got, want)
		}
	}
}
