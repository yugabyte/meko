package models_test

import (
	"testing"

	"meko/api_server/internal/models"
)

func TestEmbeddingDims_KnownModels(t *testing.T) {
	tests := []struct {
		model    string
		expected int
	}{
		{"text-embedding-ada-002", 1536},
		{"text-embedding-3-small", 1536},
		{"text-embedding-3-large", 3072},
	}

	for _, tt := range tests {
		if got := models.EmbeddingDims(tt.model); got != tt.expected {
			t.Errorf(
				"EmbeddingDims(%q) = %d, want %d",
				tt.model, got, tt.expected,
			)
		}
	}
}

func TestEmbeddingDims_UnknownModel(t *testing.T) {
	if got := models.EmbeddingDims("unknown-model"); got != 1536 {
		t.Errorf(
			"EmbeddingDims(unknown) = %d, want 1536", got,
		)
	}
}
