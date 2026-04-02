// Package k8s provides a Kubernetes client for creating MekoUserAccount
// custom resources on the EKS cluster.
package k8s

import (
	"context"
	"fmt"
	"log/slog"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
)

var mekoUserAccountGVR = schema.GroupVersionResource{
	Group:    "saas.mekodata.ai",
	Version:  "v1",
	Resource: "mekouseraccounts",
}

// MekoUserAccountParams holds the fields needed to create a
// MekoUserAccount custom resource.
type MekoUserAccountParams struct {
	AccountID      string
	Namespace      string
	AccountTier    string
	MCPImage       string
	RAGImage       string
	YugabyteDBHost string
	YugabyteDBPort int32
	YugabyteDBUser string
	YugabyteDBPass string
	YugabyteDBName string
	MCPReplicas       int32
	RAGReplicas       int32
	LangfuseSecretKey string
	LangfusePublicKey string
	LangfuseBaseURL   string
}

// Client wraps a Kubernetes dynamic client scoped to MekoUserAccount
// operations.
type Client struct {
	dynClient dynamic.Interface
}

// NewClient creates a Client using the in-cluster ServiceAccount
// credentials automatically mounted by Kubernetes.
func NewClient() (*Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("loading in-cluster config: %w", err)
	}

	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("creating dynamic client: %w", err)
	}

	slog.Info("Kubernetes client initialized (in-cluster)")
	return &Client{dynClient: dyn}, nil
}

// NewClientFromDynamic creates a Client from an existing dynamic.Interface,
// primarily for testing.
func NewClientFromDynamic(dyn dynamic.Interface) *Client {
	return &Client{dynClient: dyn}
}

// CreateMekoUserAccount builds an unstructured MekoUserAccount CR from
// the given params and creates it in the cluster.
func (c *Client) CreateMekoUserAccount(ctx context.Context, p MekoUserAccountParams) error {
	obj := buildMekoUserAccount(p)

	res := c.dynClient.Resource(mekoUserAccountGVR).Namespace(p.Namespace)
	_, err := res.Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		slog.Error("Failed to create MekoUserAccount",
			"account_id", p.AccountID,
			"namespace", p.Namespace,
			"error", err,
		)
		return fmt.Errorf("creating MekoUserAccount for %s: %w", p.AccountID, err)
	}

	slog.Info("Created MekoUserAccount",
		"account_id", p.AccountID,
		"namespace", p.Namespace,
		"name", obj.GetName(),
	)
	return nil
}

// buildMekoUserAccount constructs the unstructured object matching the
// MekoUserAccount CRD schema.
func buildMekoUserAccount(p MekoUserAccountParams) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "saas.mekodata.ai/v1",
			"kind":       "MekoUserAccount",
			"metadata": map[string]interface{}{
				"name":      "mekouseraccount-" + p.AccountID,
				"namespace": p.Namespace,
			},
			"spec": map[string]interface{}{
				"accountId":          p.AccountID,
				"accountTier":        p.AccountTier,
				"mcpImage":           p.MCPImage,
				"ragImage":           p.RAGImage,
				"yugabyteDBHost":     p.YugabyteDBHost,
				"yugabyteDBPort":     int64(p.YugabyteDBPort),
				"yugabyteDBUser":     p.YugabyteDBUser,
				"yugabyteDBPassword": p.YugabyteDBPass,
				"yugabyteDBName":     p.YugabyteDBName,
				"mcpReplicas":        int64(p.MCPReplicas),
				"ragReplicas":        int64(p.RAGReplicas),
				"langfuseSecretKey":  p.LangfuseSecretKey,
				"langfusePublicKey":  p.LangfusePublicKey,
				"langfuseBaseURL":    p.LangfuseBaseURL,
			},
		},
	}
	return obj
}
