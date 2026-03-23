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
			AccountID:     "abc123",
			AccountTier:   TierPro,
			MCPImage:      "mcp:latest",
			RAGImage:      "rag:latest",
			LangfuseImage: "langfuse:latest",
			MCPReplicas:   2,
			RAGReplicas:   1,
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
