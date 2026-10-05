// Package cognitomock stands a Cognito user pool mock up in a namespace so an
// api-server configured for Cognito can run against it with no product change.
//
// The api-server reaches Cognito in two ways, both hardcoded to the public
// hostname cognito-idp.<region>.amazonaws.com over HTTPS: the AWS SDK for the
// user pool API, and a plain HTTP client for the pool's JWKS. The mock serves
// both on one HTTPS listener with a certificate for that hostname. The
// consumer resolves the name to the mock Service (hostAliases) and trusts the
// generated CA (SSL_CERT_FILE, which Go reads on top of the system roots).
//
// A second plain HTTP listener carries the framework's control API: create a
// confirmed user, mint a token. It is unauthenticated and in-cluster only, like
// the MinIO next to it.
package cognitomock

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/yugabyte/meko/meko/test/functional/framework/apps/mockpod"
	"github.com/yugabyte/meko/meko/test/functional/framework/exec/k8s"
	"github.com/yugabyte/meko/meko/test/functional/framework/kubemeta"
)

const (
	Name       = "cognito-mock"
	SecretName = Name
	CAFile     = "ca.crt"

	DefaultRegion   = "us-east-1"
	DefaultPoolID   = "us-east-1_MekoQa001"
	DefaultClientID = "mekoqa0client0id0000000000"
	// DefaultTokenTTL outlasts a kept stack's TTL. Cognito's own default is an
	// hour, and a test that needs a short one mints it through the control API.
	DefaultTokenTTL = 24 * time.Hour

	// ConfirmationCode is what the mock accepts everywhere Cognito would have
	// emailed a code.
	ConfirmationCode = "123456"

	ServicePort = 443
	CtlPort     = 8080
	tlsPort     = 8443
)

type Spec struct {
	Region       string
	PoolID       string
	ClientID     string
	ClientSecret string
	Pod          mockpod.Config
	TokenTTL     time.Duration
	ReadyTimeout time.Duration
}

func (s Spec) WithDefaults() Spec {
	def := func(v *string, d string) {
		if *v == "" {
			*v = d
		}
	}
	def(&s.Region, DefaultRegion)
	def(&s.PoolID, DefaultPoolID)
	def(&s.ClientID, DefaultClientID)
	s.Pod = s.Pod.WithDefaults()
	if s.TokenTTL <= 0 {
		s.TokenTTL = DefaultTokenTTL
	}
	if s.ReadyTimeout <= 0 {
		s.ReadyTimeout = 5 * time.Minute
	}
	return s
}

func (s Spec) Host() string { return Host(s.WithDefaults().Region) }

func Host(region string) string { return "cognito-idp." + region + ".amazonaws.com" }

func (s Spec) Issuer() string {
	s = s.WithDefaults()
	return "https://" + Host(s.Region) + "/" + s.PoolID
}

const (
	pkiDir   = "/etc/cognito-mock"
	stateDir = "/state"

	secretClientKey = "client-secret"
	tokenKeyFile    = "token.key"
)

// The client secret is not an argument, so it stays out of the pod spec.
func (s Spec) Args() []string {
	s = s.WithDefaults()
	return []string{
		"mock", "cognito",
		"--region", s.Region, "--pool-id", s.PoolID, "--client-id", s.ClientID,
		"--token-ttl", s.TokenTTL.String(), "--code", ConfirmationCode,
		"--tls-port", fmt.Sprint(tlsPort), "--ctl-port", fmt.Sprint(CtlPort),
		"--tls-cert", pkiDir + "/tls.crt", "--tls-key", pkiDir + "/tls.key",
		"--token-key", pkiDir + "/" + tokenKeyFile,
		"--state", stateDir + "/users.json",
	}
}

func (s Spec) Manifest(m Material, bin mockpod.Binary) (string, error) {
	s = s.WithDefaults()
	meta := func(name string) map[string]any {
		return map[string]any{"name": name, "labels": labels()}
	}
	selector := map[string]any{"app": Name}
	podLabels := labels()
	podLabels[mockpod.BinLabel] = bin.Label()
	podLabels[mockpod.ConfigLabel] = s.configSHA(m)
	docs := []map[string]any{
		{
			"apiVersion": "v1", "kind": "Secret", "metadata": meta(SecretName), "type": "Opaque",
			"stringData": map[string]string{
				secretClientKey: s.ClientSecret, CAFile: string(m.CAPEM),
				"tls.crt": string(m.CertPEM), "tls.key": string(m.KeyPEM), tokenKeyFile: string(m.TokenKeyPEM),
			},
		},
		{
			"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta(Name),
			"spec": map[string]any{
				"replicas": 1,
				"selector": map[string]any{"matchLabels": selector},
				"template": map[string]any{
					"metadata": map[string]any{"labels": podLabels},
					"spec": map[string]any{
						"containers": []any{map[string]any{
							"name": "mock", "image": s.Pod.Image,
							"command": mockpod.Command(s.Args()...),
							"env": []any{map[string]any{"name": "COGNITO_MOCK_CLIENT_SECRET", "valueFrom": map[string]any{
								"secretKeyRef": map[string]any{"name": SecretName, "key": secretClientKey},
							}}},
							"ports": []any{map[string]any{"containerPort": tlsPort}, map[string]any{"containerPort": CtlPort}},
							"readinessProbe": map[string]any{
								"httpGet":       map[string]any{"path": "/healthz", "port": CtlPort},
								"periodSeconds": 3,
							},
							"resources": map[string]any{"requests": map[string]string{"cpu": "50m", "memory": "64Mi"}},
							"volumeMounts": []any{
								map[string]any{"name": "pki", "mountPath": pkiDir, "readOnly": true},
								map[string]any{"name": "state", "mountPath": stateDir},
								mockpod.VolumeMount(),
							},
						}},
						"volumes": []any{
							map[string]any{"name": "pki", "secret": map[string]any{"secretName": SecretName}},
							map[string]any{"name": "state", "emptyDir": map[string]any{}},
							mockpod.Volume(),
						},
					},
				},
			},
		},
		{
			"apiVersion": "v1", "kind": "Service", "metadata": meta(Name),
			"spec": map[string]any{
				"selector": selector,
				"ports": []any{
					map[string]any{"name": "https", "port": ServicePort, "targetPort": tlsPort},
					map[string]any{"name": "control", "port": CtlPort, "targetPort": CtlPort},
				},
			},
		},
	}
	var b strings.Builder
	for i, d := range docs {
		out, err := yaml.Marshal(d)
		if err != nil {
			return "", err
		}
		if i > 0 {
			b.WriteString("---\n")
		}
		b.Write(out)
	}
	return b.String(), nil
}

func (s Spec) configSHA(m Material) string {
	return sha(m.CertPEM, m.TokenKeyPEM, []byte(s.ClientSecret), []byte(strings.Join(s.Args(), " ")))
}

func labels() map[string]string {
	return kubemeta.TestLabels(map[string]string{"app": Name})
}

type Deployed struct {
	ClusterIP string
}

func Deploy(ctx context.Context, kube *k8s.Kubectl, s Spec, m Material) (Deployed, error) {
	s = s.WithDefaults()
	bin, err := mockpod.Resolve(ctx, s.Pod)
	if err != nil {
		return Deployed{}, fmt.Errorf("cognito mock: %w", err)
	}
	manifest, err := s.Manifest(m, bin)
	if err != nil {
		return Deployed{}, err
	}
	if err := kube.Apply(ctx, manifest); err != nil {
		return Deployed{}, fmt.Errorf("cognito mock: apply: %w", err)
	}
	if err := mockpod.Install(ctx, kube, Name, bin, s.configSHA(m), s.ReadyTimeout); err != nil {
		return Deployed{}, fmt.Errorf("cognito mock: %w", err)
	}
	if err := kube.RolloutStatus(ctx, "deployment", Name, s.ReadyTimeout); err != nil {
		return Deployed{}, fmt.Errorf("cognito mock: not ready: %w", err)
	}
	ip, err := ClusterIP(ctx, kube)
	if err != nil {
		return Deployed{}, err
	}
	return Deployed{ClusterIP: ip}, nil
}

func ClusterIP(ctx context.Context, kube *k8s.Kubectl) (string, error) {
	ip, err := kube.Get(ctx, "service", Name, "{.spec.clusterIP}")
	if err != nil || ip == "" || ip == "None" {
		return "", fmt.Errorf("cognito mock: Service has no ClusterIP: %v", err)
	}
	return ip, nil
}
