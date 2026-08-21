package main

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/yugabyte/amp/controller/internal/core"
)

// migrationEndpointLookup adapts the core backend to the task package's narrow
// VoyagerMigrationEndpointLookup port: it resolves a provisioned endpoint's
// deployed client address so the attach-target step can record it as the
// migration's target, without the task package importing the core domain layer.
type migrationEndpointLookup struct{ backend core.Backend }

func (a migrationEndpointLookup) EndpointAddress(ctx context.Context, poolID, projectID, branchID, endpointID uuid.UUID) (string, error) {
	endpoint, err := a.backend.GetEndpoint(ctx, poolID, projectID, branchID, endpointID)
	if err != nil {
		return "", err
	}
	if endpoint.Address == "" {
		return "", fmt.Errorf("endpoint %s has no deployed address", endpointID)
	}
	return endpoint.Address, nil
}
