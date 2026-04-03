package k8s

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic/fake"
)

func testParams() MekoUserAccountParams {
	return MekoUserAccountParams{
		AccountID:         "abc123",
		Namespace:         "meko-tenants",
		AccountTier:       "free",
		MCPImage:          "registry/mcp:latest",
		RAGImage:          "registry/rag:0.1.0",
		YugabyteDBHost:    "yb-tserver.yb-system.svc.cluster.local",
		YugabyteDBPort:    5433,
		YugabyteDBUser:    "db_default_abc123",
		YugabyteDBPass:    "secret-pass",
		YugabyteDBName:    "abc123_default_abc123",
		MCPReplicas:       1,
		RAGReplicas:       1,
		LangfuseSecretKey: "sk-lf-test",
		LangfusePublicKey: "pk-lf-test",
		LangfuseBaseURL:   "http://langfuse:3000",
	}
}

func TestBuildMekoUserAccount(t *testing.T) {
	p := testParams()
	obj := buildMekoUserAccount(p)

	if got := obj.GetAPIVersion(); got != "saas.mekodata.ai/v1" {
		t.Errorf("apiVersion = %q, want saas.mekodata.ai/v1", got)
	}
	if got := obj.GetKind(); got != "MekoUserAccount" {
		t.Errorf("kind = %q, want MekoUserAccount", got)
	}
	if got := obj.GetName(); got != "mekouseraccount-abc123" {
		t.Errorf("name = %q, want mekouseraccount-abc123", got)
	}
	if got := obj.GetNamespace(); got != "meko-tenants" {
		t.Errorf("namespace = %q, want meko-tenants", got)
	}

	spec, ok := obj.Object["spec"].(map[string]interface{})
	if !ok {
		t.Fatal("spec is not a map")
	}

	checks := map[string]interface{}{
		"accountId":          "abc123",
		"accountTier":        "free",
		"mcpImage":           "registry/mcp:latest",
		"ragImage":           "registry/rag:0.1.0",
		"yugabyteDBHost":     "yb-tserver.yb-system.svc.cluster.local",
		"yugabyteDBPort":     int64(5433),
		"yugabyteDBUser":     "db_default_abc123",
		"yugabyteDBPassword": "secret-pass",
		"yugabyteDBName":     "abc123_default_abc123",
		"mcpReplicas":        int64(1),
		"ragReplicas":        int64(1),
		"langfuseSecretKey":  "sk-lf-test",
		"langfusePublicKey":  "pk-lf-test",
		"langfuseBaseURL":    "http://langfuse:3000",
	}
	for key, want := range checks {
		got, exists := spec[key]
		if !exists {
			t.Errorf("spec[%q] missing", key)
			continue
		}
		if got != want {
			t.Errorf("spec[%q] = %v (%T), want %v (%T)", key, got, got, want, want)
		}
	}
}

func TestCreateMekoUserAccount_Success(t *testing.T) {
	scheme := runtime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := NewClientFromDynamic(dynClient)

	p := testParams()
	err := client.CreateMekoUserAccount(context.Background(), p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	res := dynClient.Resource(mekoUserAccountGVR).Namespace("meko-tenants")
	created, err := res.Get(context.Background(), "mekouseraccount-abc123", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get created resource: %v", err)
	}

	if got := created.GetName(); got != "mekouseraccount-abc123" {
		t.Errorf("created name = %q, want mekouseraccount-abc123", got)
	}

	spec, ok := created.Object["spec"].(map[string]interface{})
	if !ok {
		t.Fatal("created spec is not a map")
	}
	if got := spec["accountId"]; got != "abc123" {
		t.Errorf("created accountId = %v, want abc123", got)
	}
}

func TestCreateMekoUserAccount_AlreadyExists(t *testing.T) {
	scheme := runtime.NewScheme()
	dynClient := fake.NewSimpleDynamicClient(scheme)
	client := NewClientFromDynamic(dynClient)

	p := testParams()

	if err := client.CreateMekoUserAccount(context.Background(), p); err != nil {
		t.Fatalf("first create: %v", err)
	}

	err := client.CreateMekoUserAccount(context.Background(), p)
	if err == nil {
		t.Fatal("expected error on duplicate create, got nil")
	}
}
