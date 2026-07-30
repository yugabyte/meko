package apiclient

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_DoSendsAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		resp := APIResponse{Data: json.RawMessage(`{}`)}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{
		baseURL:    srv.URL,
		httpClient: srv.Client(),
		authToken:  "my-token",
	}

	if _, err := c.do(http.MethodGet, "/test", nil); err != nil {
		t.Fatalf("do: %v", err)
	}

	want := "Bearer my-token"
	if gotAuth != want {
		t.Errorf("Authorization = %q, want %q", gotAuth, want)
	}
}

func TestClient_DoOmitsAuthHeaderWhenEmpty(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		resp := APIResponse{Data: json.RawMessage(`{}`)}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{
		baseURL:    srv.URL,
		httpClient: srv.Client(),
	}

	if _, err := c.do(http.MethodGet, "/test", nil); err != nil {
		t.Fatalf("do: %v", err)
	}

	if gotAuth != "" {
		t.Errorf("Authorization = %q, want empty", gotAuth)
	}
}

func TestNewFromBaseURL(t *testing.T) {
	c := NewFromBaseURL("https://mekodev.com")
	want := "https://mekodev.com/api/v1"
	if c.baseURL != want {
		t.Errorf("baseURL = %q, want %q", c.baseURL, want)
	}
}

func TestClient_Login(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/auth/login" {
			t.Errorf("path = %s, want /auth/login", r.URL.Path)
		}

		var req LoginRequest
		json.NewDecoder(r.Body).Decode(&req)
		if req.Email != "user@test.com" {
			t.Errorf("email = %q, want user@test.com", req.Email)
		}

		resp := APIResponse{
			Data: json.RawMessage(`{
				"access_token": "acc",
				"id_token": "id",
				"refresh_token": "ref",
				"expires_in": 3600
			}`),
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{
		baseURL:    srv.URL,
		httpClient: srv.Client(),
	}

	got, err := c.Login("user@test.com", "pass123")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	if got.AccessToken != "acc" {
		t.Errorf("AccessToken = %q, want %q", got.AccessToken, "acc")
	}
	if got.ExpiresIn != 3600 {
		t.Errorf("ExpiresIn = %d, want 3600", got.ExpiresIn)
	}
}

func TestClient_LoginError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		resp := APIResponse{
			Error: &APIError{
				Code:    "UNAUTHORIZED",
				Message: "invalid credentials",
			},
		}
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := &Client{
		baseURL:    srv.URL,
		httpClient: srv.Client(),
	}

	_, err := c.Login("bad@test.com", "wrong")
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.Code != "UNAUTHORIZED" {
		t.Errorf("Code = %q, want UNAUTHORIZED", apiErr.Code)
	}
}
