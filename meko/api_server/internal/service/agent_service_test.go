package service_test

import (
	"strings"
	"testing"

	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// AgentService is wired against *repository.AgentRepo, which needs a
// real *sql.DB. The link/unlink semantics tests live in
// internal/repository/agent_repo_test.go (sqlmock-driven). Here we
// exercise the validation paths that don't reach the repo so they can
// run without a database.

func TestAgentService_CreateUserAgent_RequiresUserID(t *testing.T) {
	svc := service.NewAgentService(nil)
	_, err := svc.CreateUserAgent("", service.CreateUserAgentRequest{
		AgentID: "agent-a",
	})
	if err == nil || !strings.Contains(err.Error(), "user_id is required") {
		t.Errorf("expected 'user_id is required' error, got %v", err)
	}
}

func TestAgentService_CreateUserAgent_RequiresAgentID(t *testing.T) {
	svc := service.NewAgentService(nil)
	_, err := svc.CreateUserAgent("user-1", service.CreateUserAgentRequest{
		AgentID: "   ",
	})
	if err == nil || !strings.Contains(err.Error(), "agent_id is required") {
		t.Errorf("expected 'agent_id is required' error, got %v", err)
	}
}

func TestAgentService_DeleteUserAgent_Validates(t *testing.T) {
	svc := service.NewAgentService(nil)
	if err := svc.DeleteUserAgent("", "agent-a"); err == nil {
		t.Error("expected error when user_id empty")
	}
	if err := svc.DeleteUserAgent("user-1", ""); err == nil {
		t.Error("expected error when agent_id empty")
	}
}

func TestAgentService_LinkAgentToDatapack_Validates(t *testing.T) {
	svc := service.NewAgentService(nil)
	cases := []struct {
		name                  string
		userID, agentID, dpID string
		wantContains          string
	}{
		{"missing user", "", "agent-a", "dp-1", "user_id is required"},
		{"missing agent", "user-1", "", "dp-1", "agent_id is required"},
		{"missing dp", "user-1", "agent-a", "", "datapack_id is required"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := svc.LinkAgentToDatapack(c.userID, c.agentID, c.dpID)
			if err == nil ||
				!strings.Contains(err.Error(), c.wantContains) {
				t.Errorf("expected %q, got %v", c.wantContains, err)
			}
		})
	}
}

func TestAgentService_UnlinkAgentFromDatapack_Validates(t *testing.T) {
	svc := service.NewAgentService(nil)
	if err := svc.UnlinkAgentFromDatapack("", "agent-a", "dp-1"); err == nil {
		t.Error("expected error when user_id empty")
	}
	if err := svc.UnlinkAgentFromDatapack("user-1", "", "dp-1"); err == nil {
		t.Error("expected error when agent_id empty")
	}
	if err := svc.UnlinkAgentFromDatapack("user-1", "agent-a", ""); err == nil {
		t.Error("expected error when datapack_id empty")
	}
}

func TestAgentService_ListUserAgents_Validates(t *testing.T) {
	svc := service.NewAgentService(nil)
	if _, err := svc.ListUserAgents(""); err == nil {
		t.Error("expected error for empty user_id")
	}
}

func TestAgentService_ListAgentsForDatapack_Validates(t *testing.T) {
	svc := service.NewAgentService(nil)
	if _, err := svc.ListAgentsForDatapack("", "dp-1"); err == nil {
		t.Error("expected error when user_id empty")
	}
	if _, err := svc.ListAgentsForDatapack("user-1", ""); err == nil {
		t.Error("expected error when datapack_id empty")
	}
}

// repository.ErrAgentExists must remain a wrapped sentinel so handlers
// can use errors.Is() to map it onto a 409 response.
func TestErrAgentExists_IsSentinel(t *testing.T) {
	err := repository.ErrAgentExists
	if err == nil {
		t.Fatal("ErrAgentExists is nil")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("ErrAgentExists message = %q", err.Error())
	}
}

// repository.ErrAgentNotFound must remain a wrapped sentinel so the
// datapack service can map it onto a more descriptive error.
func TestErrAgentNotFound_IsSentinel(t *testing.T) {
	err := repository.ErrAgentNotFound
	if err == nil {
		t.Fatal("ErrAgentNotFound is nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("ErrAgentNotFound message = %q", err.Error())
	}
}

// repository.ErrAgentLinkNotFound is the sentinel returned by
// UnlinkAgentFromDatapack and is mapped onto a 404 by the agent
// handler.
func TestErrAgentLinkNotFound_IsSentinel(t *testing.T) {
	err := repository.ErrAgentLinkNotFound
	if err == nil {
		t.Fatal("ErrAgentLinkNotFound is nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("ErrAgentLinkNotFound message = %q", err.Error())
	}
}

// repository.ErrAgentNotFoundForDelete is the sentinel returned by
// DeleteAgent and is mapped onto a 404 by the agent handler.
func TestErrAgentNotFoundForDelete_IsSentinel(t *testing.T) {
	err := repository.ErrAgentNotFoundForDelete
	if err == nil {
		t.Fatal("ErrAgentNotFoundForDelete is nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("ErrAgentNotFoundForDelete message = %q", err.Error())
	}
}
