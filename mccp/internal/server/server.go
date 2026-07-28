// Package server is the MCCP control-plane API + console host.
// State is persisted through a store.Store (Postgres, or in-memory for dev).
package server

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"meko/mccp/internal/api"
	"meko/mccp/internal/store"
	"meko/mccp/web"
)

// bearer extracts a bearer token from the Authorization header.
func bearer(r *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}

// authAgent validates an env-scoped status token; on failure it writes the error and returns false.
func authAgent(s store.Store, w http.ResponseWriter, envID, token string) bool {
	if token == "" {
		writeErr(w, 401, "unauthorized", "missing bearer status token")
		return false
	}
	ok, err := s.ValidateStatusToken(envID, token)
	if err != nil {
		writeErr(w, 500, "internal", err.Error())
		return false
	}
	if !ok {
		writeErr(w, 401, "unauthorized", "invalid status token")
		return false
	}
	return true
}

// Handler builds the MCCP router over a store.
func Handler(s store.Store) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) })

	mux.HandleFunc("GET /api/v1/environments", func(w http.ResponseWriter, r *http.Request) {
		envs, err := s.List()
		if err != nil {
			writeErr(w, 500, "internal", err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"items": envs, "next_cursor": nil})
	})

	mux.HandleFunc("POST /api/v1/environments", func(w http.ResponseWriter, r *http.Request) {
		var body api.EnvironmentCreate
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name == "" {
			writeErr(w, 400, "bad_request", "name is required")
			return
		}
		placement := body.Placement
		if placement == "" {
			placement = "cloud"
		}
		state := api.StatePending
		if body.ClusterSpec != nil {
			state = api.StateClusterProvisioning
		}
		plain, hash := store.NewToken("mko_enr_")
		exp := time.Now().Add(store.EnrollTokenTTL)
		e := &api.Environment{
			ID: store.Slug(body.Name), AccountID: body.AccountID, Placement: placement, Region: body.Region,
			State: state, MaxDatapacks: 400, CreatedAt: time.Now().UTC(),
			EnrollTokenHash: hash, EnrollExpiresAt: &exp, EnrollmentToken: plain, // plaintext returned once, not persisted
		}
		if err := s.Create(e); err != nil {
			writeErr(w, 500, "internal", err.Error())
			return
		}
		hint := "helm install meko-agent meko/meko-agent --set mccp.url=$MCCP --set enrollmentToken=" + plain
		if body.ClusterSpec != nil {
			hint = "meko bootstrap provision --cloud " + body.ClusterSpec.Provider + " --region " + body.Region + " --enrollment-token " + plain
		}
		log.Printf("environment registered: %s (%s)", e.ID, state)
		writeJSON(w, 201, map[string]any{"environment": e, "enrollment_token": plain, "install_hint": hint})
	})

	mux.HandleFunc("GET /api/v1/environments/{id}", func(w http.ResponseWriter, r *http.Request) {
		e, ok, err := s.Get(r.PathValue("id"))
		if err != nil {
			writeErr(w, 500, "internal", err.Error())
			return
		}
		if !ok {
			writeErr(w, 404, "not_found", "no such environment")
			return
		}
		writeJSON(w, 200, e)
	})

	mux.HandleFunc("PATCH /api/v1/environments/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Action string `json:"action"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		ok, err := s.Action(r.PathValue("id"), body.Action)
		if err != nil {
			writeErr(w, 500, "internal", err.Error())
			return
		}
		if !ok {
			writeErr(w, 404, "not_found", "no such environment")
			return
		}
		log.Printf("environment %s action=%s", r.PathValue("id"), body.Action)
		w.WriteHeader(202)
	})

	mux.HandleFunc("DELETE /api/v1/environments/{id}", func(w http.ResponseWriter, r *http.Request) {
		ok, err := s.Action(r.PathValue("id"), "deregister")
		if err != nil {
			writeErr(w, 500, "internal", err.Error())
			return
		}
		if !ok {
			writeErr(w, 404, "not_found", "no such environment")
			return
		}
		w.WriteHeader(202)
	})

	mux.HandleFunc("GET /api/v1/datapacks", func(w http.ResponseWriter, r *http.Request) {
		dps, err := s.Datapacks(r.URL.Query().Get("envId"))
		if err != nil {
			writeErr(w, 500, "internal", err.Error())
			return
		}
		writeJSON(w, 200, dps)
	})

	mux.HandleFunc("GET /api/v1/entitlements", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, api.Entitlement{Plan: "enterprise", Source: "ybm-aeon"})
	})

	// Generic desired-state write: replace the environment's desired resource set.
	mux.HandleFunc("POST /api/v1/environments/{id}/desired", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Resources []api.DesiredResource `json:"resources"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, 400, "bad_request", "resources required")
			return
		}
		ok, err := s.SetDesiredResources(r.PathValue("id"), body.Resources)
		if err != nil {
			writeErr(w, 500, "internal", err.Error())
			return
		}
		if !ok {
			writeErr(w, 404, "not_found", "no such environment")
			return
		}
		w.WriteHeader(202)
	})

	// Convenience: set desired datapacks by name (maps to kind="datapack" resources).
	mux.HandleFunc("POST /api/v1/environments/{id}/datapacks", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Names []string `json:"names"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeErr(w, 400, "bad_request", "names required")
			return
		}
		res := make([]api.DesiredResource, 0, len(body.Names))
		for _, n := range body.Names {
			res = append(res, api.DesiredResource{Kind: "datapack", Name: n, Spec: json.RawMessage(`{"yugabytedb_secret_ref":"meko-shared-credentials"}`)})
		}
		ok, err := s.SetDesiredResources(r.PathValue("id"), res)
		if err != nil {
			writeErr(w, 500, "internal", err.Error())
			return
		}
		if !ok {
			writeErr(w, 404, "not_found", "no such environment")
			return
		}
		log.Printf("desired datapacks for %s set to %v", r.PathValue("id"), body.Names)
		w.WriteHeader(202)
	})

	// ---- agent surface ----
	mux.HandleFunc("POST /api/v1/agent/enroll", func(w http.ResponseWriter, r *http.Request) {
		var req api.EnrollRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeErr(w, 400, "bad_request", "invalid enroll request")
			return
		}
		if req.EnrollmentToken == "" {
			writeErr(w, 400, "bad_request", "enrollment_token required")
			return
		}
		placement := req.Placement
		if placement == "" {
			placement = "cloud"
		}
		e, statusToken, ok, err := s.EnrollWithToken(req.EnrollmentToken, placement, req.ClusterFacts)
		if err != nil {
			writeErr(w, 500, "internal", err.Error())
			return
		}
		if !ok {
			writeErr(w, 401, "unauthorized", "invalid or already-used enrollment token")
			return
		}
		log.Printf("agent enrolled: env=%s provider=%s k8s=%s nodes=%d", e.ID, req.ClusterFacts.Provider, req.ClusterFacts.K8sVersion, req.ClusterFacts.NodeCount)
		writeJSON(w, 200, api.EnrollResponse{EnvironmentID: e.ID, StatusToken: statusToken, MCPBaseURL: e.MCPBaseURL})
	})

	mux.HandleFunc("GET /api/v1/agent/desired-state", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("envId")
		if id == "" {
			writeErr(w, 400, "bad_request", "envId query param required")
			return
		}
		if !authAgent(s, w, id, bearer(r)) {
			return
		}
		rev, _ := s.Revision()
		res, err := s.DesiredResources(id)
		if err != nil {
			writeErr(w, 500, "internal", err.Error())
			return
		}
		if res == nil {
			res = []api.DesiredResource{}
		}
		writeJSON(w, 200, api.DesiredState{
			Revision:       rev,
			TargetVersions: map[string]string{"agent": "v1.4.2", "mcp": "v1.4.2", "rag": "v1.4.2"},
			Resources:      res,
		})
	})

	mux.HandleFunc("POST /api/v1/agent/status", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Query().Get("envId")
		var rep api.StatusReport
		if err := json.NewDecoder(r.Body).Decode(&rep); err != nil {
			writeErr(w, 400, "bad_request", "invalid status report")
			return
		}
		if id == "" {
			writeErr(w, 400, "bad_request", "envId query param required")
			return
		}
		if !authAgent(s, w, id, bearer(r)) {
			return
		}
		ok, err := s.ApplyStatus(id, rep)
		if err != nil {
			writeErr(w, 500, "internal", err.Error())
			return
		}
		if !ok {
			writeErr(w, 404, "not_found", "unknown environment "+id)
			return
		}
		w.WriteHeader(204)
	})

	mux.HandleFunc("POST /api/v1/agent/bootstrap-progress", func(w http.ResponseWriter, r *http.Request) {
		var bp api.BootstrapProgress
		json.NewDecoder(r.Body).Decode(&bp)
		if id := r.URL.Query().Get("envId"); id != "" && bp.Phase != "" {
			s.SetState(id, bp.Phase)
		}
		log.Printf("bootstrap-progress: phase=%s step=%s", bp.Phase, bp.Step)
		w.WriteHeader(204)
	})

	// ---- console ----
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, err := web.FS.ReadFile("console.html")
		if err != nil {
			http.Error(w, "console not embedded", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write(b)
	})

	return logMiddleware(mux)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, ecode, msg string) {
	writeJSON(w, code, api.APIError{Code: ecode, Message: msg})
}

func logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			log.Printf("%s %s", r.Method, r.URL.Path)
		}
		next.ServeHTTP(w, r)
	})
}
