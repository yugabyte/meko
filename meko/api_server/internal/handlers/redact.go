package handlers

import (
	"meko/api_server/internal/models"
	"meko/api_server/internal/repository"
	"meko/api_server/internal/service"
)

// redactProvisionDetails clears connection details from
// DefaultDatapackDetails before the response is sent to any client.
func redactProvisionDetails(d *repository.DefaultDatapackDetails) {
	if d == nil {
		return
	}
	d.DBUserName = ""
	d.DBPassword = ""
	d.DBConnStr = ""
}

// redactDatapack clears connection details from a Datapack model
// before the response is sent to any client.
func redactDatapack(dp *models.Datapack) {
	if dp == nil {
		return
	}
	dp.UniverseID = ""
	dp.YBUniverseConn = ""
	dp.MCPUserName = ""
}

// redactDatapacks clears connection details from a slice of Datapack
// models before the response is sent to any client.
func redactDatapacks(dps []models.Datapack) {
	for i := range dps {
		dps[i].UniverseID = ""
		dps[i].YBUniverseConn = ""
		dps[i].MCPUserName = ""
	}
}

// redactCreateResponse clears connection details from a CreateResponse
// before the response is sent to any client.
func redactCreateResponse(r *service.CreateResponse) {
	if r == nil {
		return
	}
	r.MCPUserName = ""
	r.MCPServer.APIKey = ""
	r.MCPServer.URL = ""
	r.DBAccess.UserName = ""
	r.DBAccess.Password = ""
	r.DBAccess.ConnStr = ""
}

// redactDatapackStatus clears connection details from a DatapackStatus
// before the response is sent to any client.
func redactDatapackStatus(s *repository.DatapackStatus) {
	if s == nil {
		return
	}
	s.UniverseID = ""
	s.YBUniverseConn = ""
	s.MCPUserName = ""
}
