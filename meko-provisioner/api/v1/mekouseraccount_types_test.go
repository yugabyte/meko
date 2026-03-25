package v1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestAccountTierConstants(t *testing.T) {
	tests := []struct {
		tier AccountTier
		want string
	}{
		{TierFree, "free"},
		{TierPro, "pro"},
		{TierEnterprise, "enterprise"},
	}
	for _, tt := range tests {
		if string(tt.tier) != tt.want {
			t.Errorf("AccountTier %q != %q", tt.tier, tt.want)
		}
	}
}

func TestMekoUserAccountDeepCopy(t *testing.T) {
	original := &MekoUserAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-user",
			Namespace: "meko-tenants",
		},
		Spec: MekoUserAccountSpec{
			AccountID:          "abc123",
			AccountTier:        TierPro,
			MCPImage:           "mcp:latest",
			RAGImage:           "rag:latest",
			LangfuseImage:      "langfuse:latest",
			YugabyteDBHost:     "yb-tserver.yb-system.svc.cluster.local",
			YugabyteDBPort:     5433,
			YugabyteDBUser:     "yugabyte",
			YugabyteDBPassword: "testpass",
			YugabyteDBName:     "meko_abc123",
			MCPReplicas:        2,
			RAGReplicas:        1,
		},
		Status: MekoUserAccountStatus{
			Phase:         "Active",
			NamespaceName: "tenant-abc123",
			Conditions: []metav1.Condition{
				{
					Type:   "Available",
					Status: metav1.ConditionTrue,
					Reason: "Provisioned",
				},
			},
		},
	}

	copied := original.DeepCopy()

	if copied.Name != original.Name {
		t.Errorf("DeepCopy Name: got %q, want %q", copied.Name, original.Name)
	}
	if copied.Spec.AccountID != original.Spec.AccountID {
		t.Errorf("DeepCopy AccountID: got %q, want %q", copied.Spec.AccountID, original.Spec.AccountID)
	}
	if copied.Spec.MCPReplicas != original.Spec.MCPReplicas {
		t.Errorf("DeepCopy MCPReplicas: got %d, want %d", copied.Spec.MCPReplicas, original.Spec.MCPReplicas)
	}
	if copied.Spec.YugabyteDBHost != original.Spec.YugabyteDBHost {
		t.Errorf("DeepCopy YugabyteDBHost: got %q, want %q",
			copied.Spec.YugabyteDBHost, original.Spec.YugabyteDBHost)
	}
	if copied.Spec.YugabyteDBPassword != original.Spec.YugabyteDBPassword {
		t.Errorf("DeepCopy YugabyteDBPassword: got %q, want %q",
			copied.Spec.YugabyteDBPassword, original.Spec.YugabyteDBPassword)
	}
	if copied.Status.Phase != original.Status.Phase {
		t.Errorf("DeepCopy Phase: got %q, want %q", copied.Status.Phase, original.Status.Phase)
	}
	if len(copied.Status.Conditions) != 1 {
		t.Fatalf("DeepCopy Conditions length: got %d, want 1", len(copied.Status.Conditions))
	}

	// Mutating the copy must not affect the original.
	const changed = "changed-value"
	copied.Spec.AccountID = changed
	copied.Status.Conditions[0].Reason = changed
	if original.Spec.AccountID == changed {
		t.Error("DeepCopy did not isolate Spec")
	}
	if original.Status.Conditions[0].Reason == changed {
		t.Error("DeepCopy did not isolate Status.Conditions")
	}
}

func TestYugabyteDBConnectionStringURI(t *testing.T) {
	spec := &MekoUserAccountSpec{
		YugabyteDBHost:     "yb-tserver",
		YugabyteDBPort:     5433,
		YugabyteDBUser:     "yugabyte",
		YugabyteDBPassword: "secret",
		YugabyteDBName:     "mydb",
	}
	want := "postgresql://yugabyte:secret@yb-tserver:5433/mydb"
	if got := spec.YugabyteDBConnectionStringURI(); got != want {
		t.Errorf("YugabyteDBConnectionStringURI() = %q, want %q", got, want)
	}
}

func TestYugabyteDBConnectionStringLibpq(t *testing.T) {
	spec := &MekoUserAccountSpec{
		YugabyteDBHost:     "yb-tserver",
		YugabyteDBPort:     5433,
		YugabyteDBUser:     "yugabyte",
		YugabyteDBPassword: "secret",
		YugabyteDBName:     "mydb",
	}
	want := "host=yb-tserver port=5433 dbname=mydb user=yugabyte password=secret"
	if got := spec.YugabyteDBConnectionStringLibpq(); got != want {
		t.Errorf("YugabyteDBConnectionStringLibpq() = %q, want %q", got, want)
	}
}

func TestGetYugabyteDBPortDefault(t *testing.T) {
	spec := &MekoUserAccountSpec{}
	if got := spec.GetYugabyteDBPort(); got != 5433 {
		t.Errorf("GetYugabyteDBPort() with zero = %d, want 5433", got)
	}
	spec.YugabyteDBPort = 9999
	if got := spec.GetYugabyteDBPort(); got != 9999 {
		t.Errorf("GetYugabyteDBPort() with 9999 = %d, want 9999", got)
	}
}

func TestGetYugabyteDBUserDefault(t *testing.T) {
	spec := &MekoUserAccountSpec{}
	if got := spec.GetYugabyteDBUser(); got != "yugabyte" {
		t.Errorf("GetYugabyteDBUser() with empty = %q, want %q", got, "yugabyte")
	}
	spec.YugabyteDBUser = "custom"
	if got := spec.GetYugabyteDBUser(); got != "custom" {
		t.Errorf("GetYugabyteDBUser() with custom = %q, want %q", got, "custom")
	}
}

func TestMekoUserAccountDeepCopyObject(t *testing.T) {
	original := &MekoUserAccount{
		Spec: MekoUserAccountSpec{
			AccountID:   "xyz",
			AccountTier: TierFree,
		},
	}
	obj := original.DeepCopyObject()
	if _, ok := obj.(*MekoUserAccount); !ok {
		t.Errorf("DeepCopyObject returned %T, want *MekoUserAccount", obj)
	}
}
