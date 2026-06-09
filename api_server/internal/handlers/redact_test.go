package handlers

import (
	"testing"

	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

func TestRedactProvisionDetails(t *testing.T) {
	d := &repository.DefaultDatapackDetails{
		DatapackID:   "dp-1",
		DatapackName: "test",
		MCPServerURL: "https://mcp.example.com",
		MCPAPIKey:    "bearer-token",
		DBUserName:   "mcp_user",
		DBPassword:   "real-password",
		DBConnStr:    "postgresql://mcp_user:real-password@host:5433/db",
	}

	redactProvisionDetails(d)

	if d.DBUserName != "" {
		t.Errorf("DBUserName = %q, want empty", d.DBUserName)
	}
	if d.DBPassword != "" {
		t.Errorf("DBPassword = %q, want empty", d.DBPassword)
	}
	if d.DBConnStr != "" {
		t.Errorf("DBConnStr = %q, want empty", d.DBConnStr)
	}
	if d.MCPServerURL != "https://mcp.example.com" {
		t.Errorf("MCPServerURL should be unchanged")
	}
	if d.MCPAPIKey != "bearer-token" {
		t.Errorf("MCPAPIKey should be unchanged")
	}
}

func TestRedactProvisionDetails_Nil(t *testing.T) {
	redactProvisionDetails(nil)
}

func TestRedactDatapack(t *testing.T) {
	dp := &models.Datapack{
		ID:             "dp-1",
		Name:           "test",
		UserID:         "owner-user",
		UniverseID:     "universe-123",
		YBUniverseConn: "postgresql://admin:secret@host:5433/yugabyte",
		MCPUserName:    "mcp_user",
	}

	redactDatapack(dp)

	if dp.UniverseID != "" {
		t.Errorf("UniverseID = %q, want empty", dp.UniverseID)
	}
	if dp.YBUniverseConn != "" {
		t.Errorf("YBUniverseConn = %q, want empty", dp.YBUniverseConn)
	}
	if dp.MCPUserName != "" {
		t.Errorf("MCPUserName = %q, want empty", dp.MCPUserName)
	}
	if dp.Name != "test" {
		t.Errorf("Name should be unchanged")
	}
}

func TestRedactDatapack_Nil(t *testing.T) {
	redactDatapack(nil)
}

func TestRedactDatapacks(t *testing.T) {
	dps := []models.Datapack{
		{
			UserID:         "user-1",
			UniverseID:     "u1",
			YBUniverseConn: "postgresql://u1:p1@host:5433/db",
			MCPUserName:    "mcp_u1",
		},
		{
			UserID:         "user-2",
			UniverseID:     "u2",
			YBUniverseConn: "postgresql://u2:p2@host:5433/db",
			MCPUserName:    "mcp_u2",
		},
	}

	redactDatapacks(dps)

	for i, dp := range dps {
		if dp.UniverseID != "" {
			t.Errorf("dps[%d].UniverseID = %q, want empty", i, dp.UniverseID)
		}
		if dp.YBUniverseConn != "" {
			t.Errorf("dps[%d].YBUniverseConn = %q, want empty", i, dp.YBUniverseConn)
		}
		if dp.MCPUserName != "" {
			t.Errorf("dps[%d].MCPUserName = %q, want empty", i, dp.MCPUserName)
		}
	}
}

func TestRedactCreateResponse(t *testing.T) {
	r := &service.CreateResponse{
		DatapackID:  "dp-1",
		AgentID:     "agent-1",
		Name:        "test",
		MCPUserName: "mcp_user",
		MCPServer: service.MCPServerInfo{
			URL:    "https://mcp.example.com",
			APIKey: "secret-key",
		},
		DBAccess: service.DBAccessInfo{
			UserName: "mcp_user",
			Password: "db-pass",
			ConnStr:  "postgresql://mcp_user:db-pass@host:5433/db",
		},
	}

	redactCreateResponse(r)

	if r.MCPUserName != "" {
		t.Errorf("MCPUserName = %q, want empty", r.MCPUserName)
	}
	if r.DBAccess.UserName != "" {
		t.Errorf("DBAccess.UserName = %q, want empty", r.DBAccess.UserName)
	}
	if r.DBAccess.Password != "" {
		t.Errorf("DBAccess.Password = %q, want empty", r.DBAccess.Password)
	}
	if r.DBAccess.ConnStr != "" {
		t.Errorf("DBAccess.ConnStr = %q, want empty", r.DBAccess.ConnStr)
	}
	if r.MCPServer.URL != "" {
		t.Errorf("MCPServer.URL = %q, want empty", r.MCPServer.URL)
	}
	if r.MCPServer.APIKey != "" {
		t.Errorf("MCPServer.APIKey = %q, want empty", r.MCPServer.APIKey)
	}
}

func TestRedactDatapackStatus(t *testing.T) {
	s := &repository.DatapackStatus{
		Name:           "test",
		ID:             "dp-1",
		UniverseID:     "universe-1",
		YBUniverseConn: "postgresql://admin:pw@host:5433/db",
		MCPUserName:    "admin",
		AgentCount:     3,
		KBCount:        2,
	}

	redactDatapackStatus(s)

	if s.UniverseID != "" {
		t.Errorf("UniverseID = %q, want empty", s.UniverseID)
	}
	if s.YBUniverseConn != "" {
		t.Errorf("YBUniverseConn = %q, want empty", s.YBUniverseConn)
	}
	if s.MCPUserName != "" {
		t.Errorf("MCPUserName = %q, want empty", s.MCPUserName)
	}
	if s.Name != "test" {
		t.Errorf("Name should be unchanged")
	}
	if s.AgentCount != 3 {
		t.Errorf("AgentCount should be unchanged")
	}
}
