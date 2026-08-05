package migration

import (
	"encoding/json"
	"fmt"
)

// AssessmentPayloadVersion is the minimum Voyager YugabyteD assessment payload
// version projected by this package. Newer 1.x payloads are additive and
// backward-compatible, so any 1.x at or above this minor is accepted.
const AssessmentPayloadVersion = "1.8"

const (
	assessmentPayloadMajor = 1
	assessmentPayloadMinor = 8
)

// ParseAssessmentPayload parses and projects Voyager's
// AssessMigrationPayloadYugabyteD v1.8 JSON. The JSON names intentionally match
// the upstream Go fields, including fields whose tags are implicit.
func ParseAssessmentPayload(data []byte) (Assessment, error) {
	var payload assessMigrationPayloadYugabyteD
	if err := json.Unmarshal(data, &payload); err != nil {
		return Assessment{}, fmt.Errorf("decode Voyager assessment payload: %w", err)
	}
	if !supportedAssessmentVersion(payload.PayloadVersion) {
		return Assessment{}, fmt.Errorf("unsupported Voyager assessment payload version %q (need %d.%d+ within major %d)",
			payload.PayloadVersion, assessmentPayloadMajor, assessmentPayloadMinor, assessmentPayloadMajor)
	}

	issues := make([]AssessmentIssue, 0, len(payload.AssessmentIssues))
	for _, issue := range payload.AssessmentIssues {
		issues = append(issues, AssessmentIssue{
			Category:    issue.Category,
			Type:        issue.Type,
			Name:        issue.Name,
			Description: issue.Description,
			Impact:      issue.Impact,
			ObjectType:  issue.ObjectType,
			ObjectName:  issue.ObjectName,
			DocsLink:    issue.DocsLink,
		})
	}

	notes := append([]string(nil), payload.GeneralNotes...)
	notes = append(notes, payload.ColocatedShardedNotes...)
	notes = append(notes, payload.SizingNotes...)
	if len(notes) == 0 {
		notes = append(notes, payload.Notes...)
	}
	if notes == nil {
		notes = []string{}
	}

	assessment := Assessment{
		VoyagerVersion:                 payload.VoyagerVersion,
		MigrationComplexity:            payload.MigrationComplexity,
		MigrationComplexityExplanation: payload.MigrationComplexityExplanation,
		SourceDatabase: SourceDBStats{
			TotalTableSizeBytes: payload.SourceSizeDetails.TotalTableSize,
			TotalIndexSizeBytes: payload.SourceSizeDetails.TotalIndexSize,
			TableRowCount:       payload.SourceSizeDetails.TotalTableRowCount,
		},
		Issues: issues,
		Notes:  notes,
	}
	if payload.Sizing != nil {
		rec := payload.Sizing.SizingRecommendation
		assessment.SizingRecommendation = SizingRecommendation{
			NumNodes:        rec.NumNodes,
			VCPUPerNode:     rec.VCPUsPerInstance,
			MemoryPerNodeGB: rec.MemoryPerInstance,
			ColocatedTables: append([]string(nil), rec.ColocatedTables...),
			ShardedTables:   append([]string(nil), rec.ShardedTables...),
			Summary:         sizingSummary(*payload.Sizing),
		}
	}
	if assessment.Issues == nil {
		assessment.Issues = []AssessmentIssue{}
	}
	if assessment.SizingRecommendation.ColocatedTables == nil {
		assessment.SizingRecommendation.ColocatedTables = []string{}
	}
	if assessment.SizingRecommendation.ShardedTables == nil {
		assessment.SizingRecommendation.ShardedTables = []string{}
	}
	return assessment, nil
}

// supportedAssessmentVersion accepts any 1.x payload at or above the documented
// minimum (additive, backward-compatible), and rejects older minors or a
// different major (a major bump signals a breaking change we must re-review).
func supportedAssessmentVersion(v string) bool {
	var major, minor int
	if _, err := fmt.Sscanf(v, "%d.%d", &major, &minor); err != nil {
		return false
	}
	return major == assessmentPayloadMajor && minor >= assessmentPayloadMinor
}

func sizingSummary(sizing sizingAssessmentReport) string {
	if sizing.FailureReasoning != "" {
		return sizing.FailureReasoning
	}
	if sizing.SizingRecommendation.ColocatedReasoning != "" {
		return sizing.SizingRecommendation.ColocatedReasoning
	}
	rec := sizing.SizingRecommendation
	if rec.NumNodes == 0 && rec.VCPUsPerInstance == 0 && rec.MemoryPerInstance == 0 {
		return ""
	}
	return fmt.Sprintf("%.0f node(s), %d vCPU and %d GB memory per node",
		rec.NumNodes, rec.VCPUsPerInstance, rec.MemoryPerInstance)
}

type assessMigrationPayloadYugabyteD struct {
	PayloadVersion                 string                     `json:"PayloadVersion"`
	VoyagerVersion                 string                     `json:"VoyagerVersion"`
	MigrationComplexity            string                     `json:"MigrationComplexity"`
	MigrationComplexityExplanation string                     `json:"MigrationComplexityExplanation"`
	AssessmentIssues               []assessmentIssueYugabyteD `json:"AssessmentIssues"`
	SourceSizeDetails              sourceDBSizeDetails        `json:"SourceSizeDetails"`
	Sizing                         *sizingAssessmentReport    `json:"Sizing"`
	GeneralNotes                   []string                   `json:"GeneralNotes"`
	ColocatedShardedNotes          []string                   `json:"ColocatedShardedNotes"`
	SizingNotes                    []string                   `json:"SizingNotes"`
	Notes                          []string                   `json:"Notes"`
}

type assessmentIssueYugabyteD struct {
	Category    string `json:"Category"`
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Description string `json:"Description"`
	Impact      string `json:"Impact"`
	ObjectType  string `json:"ObjectType"`
	ObjectName  string `json:"ObjectName"`
	DocsLink    string `json:"DocsLink"`
}

type sourceDBSizeDetails struct {
	TotalDBSize        int64 `json:"TotalDBSize"`
	TotalTableSize     int64 `json:"TotalTableSize"`
	TotalIndexSize     int64 `json:"TotalIndexSize"`
	TotalTableRowCount int64 `json:"TotalTableRowCount"`
}

type sizingAssessmentReport struct {
	SizingRecommendation sizingRecommendation `json:"SizingRecommendation"`
	FailureReasoning     string               `json:"FailureReasoning"`
}

type sizingRecommendation struct {
	ColocatedTables                                    []string `json:"ColocatedTables"`
	ColocatedReasoning                                 string   `json:"ColocatedReasoning"`
	ShardedTables                                      []string `json:"ShardedTables"`
	NumNodes                                           float64  `json:"NumNodes"`
	VCPUsPerInstance                                   int      `json:"VCPUsPerInstance"`
	MemoryPerInstance                                  int      `json:"MemoryPerInstance"`
	OptimalSelectConnectionsPerNode                    int64    `json:"OptimalSelectConnectionsPerNode"`
	OptimalInsertConnectionsPerNode                    int64    `json:"OptimalInsertConnectionsPerNode"`
	EstimatedTimeInMinForImport                        float64  `json:"EstimatedTimeInMinForImport"`
	EstimatedTimeInMinForImportWithoutRedundantIndexes float64  `json:"EstimatedTimeInMinForImportWithoutRedundantIndexes"`
}
