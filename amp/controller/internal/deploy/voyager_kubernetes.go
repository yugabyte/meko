package deploy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const voyagerServiceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// The Voyager image runs as the unprivileged `yugabyte` user (uid/gid 1000; see
// deploy/voyager/Dockerfile). A freshly provisioned PVC mounts root-owned, so the
// pod must declare a matching fsGroup — Kubernetes then chowns the volume to that
// group — otherwise the first write to the export dir fails with EACCES.
const (
	voyagerRunAsUser  = int64(1000)
	voyagerRunAsGroup = int64(1000)
	voyagerFSGroup    = int64(1000)
)

// KubernetesVoyagerConfig configures Voyager jobs submitted to an in-cluster
// Kubernetes API.
type KubernetesVoyagerConfig struct {
	Image              string
	Namespace          string
	PVCSize            string
	StorageClass       string
	PullSecret         string
	DBConnectionString string
}

// KubernetesVoyagerJobRunner materializes Voyager runs as PVC-backed batch/v1
// Jobs using the authenticated in-cluster REST API, without client-go.
type KubernetesVoyagerJobRunner struct {
	cfg KubernetesVoyagerConfig
}

// NewKubernetesVoyagerJobRunner constructs the Kubernetes implementation.
func NewKubernetesVoyagerJobRunner(cfg KubernetesVoyagerConfig) *KubernetesVoyagerJobRunner {
	return &KubernetesVoyagerJobRunner{cfg: cfg}
}

var _ VoyagerJobRunner = (*KubernetesVoyagerJobRunner)(nil)

func (r *KubernetesVoyagerJobRunner) StartRun(ctx context.Context, spec VoyagerRunSpec) (RunRef, error) {
	job, err := buildVoyagerJobSpec(spec, r.cfg.DBConnectionString)
	if err != nil {
		return RunRef{}, err
	}
	if err := validateKubernetesVoyagerConfig(r.cfg); err != nil {
		return RunRef{}, err
	}
	api, err := r.kubernetesAPI()
	if err != nil {
		return RunRef{}, err
	}
	if err := r.ensurePVC(ctx, api, job.Volume); err != nil {
		return RunRef{}, err
	}
	uid, err := r.ensureJob(ctx, api, job)
	if err != nil {
		return RunRef{}, err
	}
	return RunRef{
		DeployerType: VoyagerDeployerKubernetes,
		Namespace:    r.cfg.Namespace,
		JobName:      job.Name,
		JobUID:       uid,
		Volume:       job.Volume,
	}, nil
}

func (r *KubernetesVoyagerJobRunner) RunStatus(ctx context.Context, ref RunRef) (RunState, error) {
	if ref.DeployerType != VoyagerDeployerKubernetes || ref.Namespace == "" || ref.JobName == "" {
		return "", errors.New("invalid Kubernetes Voyager run reference")
	}
	api, err := r.kubernetesAPI()
	if err != nil {
		return "", err
	}
	path := fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs/%s",
		url.PathEscape(ref.Namespace), url.PathEscape(ref.JobName))
	status, body, err := api.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("get Kubernetes Voyager job: unexpected HTTP status %d", status)
	}
	return parseKubernetesJobState(body)
}

func validateKubernetesVoyagerConfig(cfg KubernetesVoyagerConfig) error {
	if cfg.Image == "" {
		return errors.New("Kubernetes Voyager image is required")
	}
	if cfg.Namespace == "" || strings.Contains(cfg.Namespace, "/") {
		return errors.New("valid Kubernetes Voyager namespace is required")
	}
	if cfg.PVCSize == "" {
		return errors.New("Kubernetes Voyager PVC size is required")
	}
	return nil
}

func (r *KubernetesVoyagerJobRunner) kubernetesAPI() (*voyagerKubernetesAPI, error) {
	return newInClusterVoyagerKubernetesAPI()
}

func (r *KubernetesVoyagerJobRunner) ensurePVC(
	ctx context.Context,
	api *voyagerKubernetesAPI,
	volume string,
) error {
	namespace := url.PathEscape(r.cfg.Namespace)
	name := url.PathEscape(volume)
	itemPath := fmt.Sprintf("/api/v1/namespaces/%s/persistentvolumeclaims/%s", namespace, name)
	status, _, err := api.do(ctx, http.MethodGet, itemPath, nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusNotFound:
	default:
		return fmt.Errorf("get Kubernetes Voyager PVC: unexpected HTTP status %d", status)
	}

	manifest := kubernetesPVC{
		APIVersion: "v1",
		Kind:       "PersistentVolumeClaim",
		Metadata: kubernetesObjectMeta{
			Name:      volume,
			Namespace: r.cfg.Namespace,
		},
		Spec: kubernetesPVCSpec{
			AccessModes:      []string{"ReadWriteOnce"},
			StorageClassName: r.cfg.StorageClass,
			Resources: kubernetesPVCResources{
				Requests: map[string]string{"storage": r.cfg.PVCSize},
			},
		},
	}
	collectionPath := fmt.Sprintf("/api/v1/namespaces/%s/persistentvolumeclaims", namespace)
	status, _, err = api.do(ctx, http.MethodPost, collectionPath, manifest)
	if err != nil {
		return err
	}
	if status != http.StatusCreated && status != http.StatusConflict {
		return fmt.Errorf("create Kubernetes Voyager PVC: unexpected HTTP status %d", status)
	}
	return nil
}

func (r *KubernetesVoyagerJobRunner) ensureJob(
	ctx context.Context,
	api *voyagerKubernetesAPI,
	job voyagerJobSpec,
) (string, error) {
	namespace := url.PathEscape(r.cfg.Namespace)
	itemPath := fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs/%s",
		namespace, url.PathEscape(job.Name))
	status, body, err := api.do(ctx, http.MethodGet, itemPath, nil)
	if err != nil {
		return "", err
	}
	if status == http.StatusOK {
		return kubernetesObjectUID(body)
	}
	if status != http.StatusNotFound {
		return "", fmt.Errorf("get Kubernetes Voyager job: unexpected HTTP status %d", status)
	}

	manifest := r.jobManifest(job)
	collectionPath := fmt.Sprintf("/apis/batch/v1/namespaces/%s/jobs", namespace)
	status, body, err = api.do(ctx, http.MethodPost, collectionPath, manifest)
	if err != nil {
		return "", err
	}
	if status == http.StatusCreated {
		return kubernetesObjectUID(body)
	}
	if status != http.StatusConflict {
		return "", fmt.Errorf("create Kubernetes Voyager job: unexpected HTTP status %d", status)
	}

	// A concurrent replay may have won the create. Read and return that object.
	status, body, err = api.do(ctx, http.MethodGet, itemPath, nil)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("get replayed Kubernetes Voyager job: unexpected HTTP status %d", status)
	}
	return kubernetesObjectUID(body)
}

func (r *KubernetesVoyagerJobRunner) jobManifest(job voyagerJobSpec) kubernetesJob {
	backoffLimit := int32(0)
	runAsUser := voyagerRunAsUser
	runAsGroup := voyagerRunAsGroup
	fsGroup := voyagerFSGroup
	runAsNonRoot := true
	podSpec := kubernetesPodSpec{
		RestartPolicy: "Never",
		SecurityContext: &kubernetesPodSecurityContext{
			RunAsUser:    &runAsUser,
			RunAsGroup:   &runAsGroup,
			RunAsNonRoot: &runAsNonRoot,
			FSGroup:      &fsGroup,
		},
		Containers: []kubernetesContainer{{
			Name:         "voyager",
			Image:        r.cfg.Image,
			Command:      []string{"/bin/bash", "-c", job.Command},
			Env:          job.Env,
			VolumeMounts: []kubernetesVolumeMount{{Name: "export", MountPath: voyagerExportDir}},
		}},
		Volumes: []kubernetesVolume{{
			Name: "export",
			PersistentVolumeClaim: kubernetesPVCVolumeSource{
				ClaimName: job.Volume,
			},
		}},
	}
	if r.cfg.PullSecret != "" {
		podSpec.ImagePullSecrets = []kubernetesLocalObjectReference{{Name: r.cfg.PullSecret}}
	}
	return kubernetesJob{
		APIVersion: "batch/v1",
		Kind:       "Job",
		Metadata: kubernetesObjectMeta{
			Name:      job.Name,
			Namespace: r.cfg.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":         "yb-voyager",
				"voyager.yugabyte.com/migration": job.MigrationID.String(),
			},
		},
		Spec: kubernetesJobSpec{
			BackoffLimit: &backoffLimit,
			Template: kubernetesPodTemplate{
				Metadata: kubernetesObjectMeta{
					Labels: map[string]string{
						"app.kubernetes.io/name":         "yb-voyager",
						"voyager.yugabyte.com/migration": job.MigrationID.String(),
					},
				},
				Spec: podSpec,
			},
		},
	}
}

func kubernetesObjectUID(body []byte) (string, error) {
	var object struct {
		Metadata struct {
			UID string `json:"uid"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &object); err != nil {
		return "", errors.New("decode Kubernetes Voyager object metadata")
	}
	if object.Metadata.UID == "" {
		return "", errors.New("Kubernetes Voyager object has no UID")
	}
	return object.Metadata.UID, nil
}

func parseKubernetesJobState(body []byte) (RunState, error) {
	var job struct {
		Status struct {
			Active     int `json:"active"`
			Succeeded  int `json:"succeeded"`
			Failed     int `json:"failed"`
			Conditions []struct {
				Type   string `json:"type"`
				Status string `json:"status"`
			} `json:"conditions"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &job); err != nil {
		return "", errors.New("decode Kubernetes Voyager job status")
	}

	complete, failed := false, false
	for _, condition := range job.Status.Conditions {
		if condition.Status != "True" {
			continue
		}
		switch condition.Type {
		case "Complete":
			complete = true
		case "Failed":
			failed = true
		}
	}
	switch {
	case complete || job.Status.Succeeded > 0:
		return RunStateSucceeded, nil
	case failed || job.Status.Failed > 0:
		return RunStateFailed, nil
	case job.Status.Active > 0:
		return RunStateRunning, nil
	default:
		return RunStatePending, nil
	}
}

type voyagerKubernetesAPI struct {
	baseURL string
}

// TODO(voyager): Extract this in-cluster API setup with KubernetesDeployer's
// equivalent helpers so controller components share one authenticated client
// implementation.
func newInClusterVoyagerKubernetesAPI() (*voyagerKubernetesAPI, error) {
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("Kubernetes in-cluster API address is unavailable")
	}
	return &voyagerKubernetesAPI{
		baseURL: "https://" + net.JoinHostPort(host, port),
	}, nil
}

// clientAndToken reloads the mounted credentials for every request. Kubernetes
// rotates projected ServiceAccount tokens on disk during a controller's
// lifetime; caching either the first token or a transient read failure would
// turn a healthy long-running runner into a permanent 401/error source.
func (a *voyagerKubernetesAPI) clientAndToken() (*http.Client, string, error) {
	token, err := os.ReadFile(voyagerServiceAccountDir + "/token")
	if err != nil {
		return nil, "", errors.New("read Kubernetes service-account token")
	}
	caPEM, err := os.ReadFile(voyagerServiceAccountDir + "/ca.crt")
	if err != nil {
		return nil, "", errors.New("read Kubernetes service-account CA")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, "", errors.New("parse Kubernetes service-account CA")
	}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			RootCAs:    pool,
		}},
	}, strings.TrimSpace(string(token)), nil
}

func (a *voyagerKubernetesAPI) do(
	ctx context.Context,
	method string,
	path string,
	body any,
) (int, []byte, error) {
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, nil, errors.New("encode Kubernetes Voyager request")
		}
		requestBody = bytes.NewReader(encoded)
	}
	client, token, err := a.clientAndToken()
	if err != nil {
		return 0, nil, err
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, method, a.baseURL+path, requestBody)
	if err != nil {
		return 0, nil, errors.New("build Kubernetes Voyager request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("execute Kubernetes Voyager request: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return 0, nil, errors.New("read Kubernetes Voyager response")
	}
	return resp.StatusCode, responseBody, nil
}

type kubernetesObjectMeta struct {
	Name      string            `json:"name,omitempty"`
	Namespace string            `json:"namespace,omitempty"`
	UID       string            `json:"uid,omitempty"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type kubernetesPVC struct {
	APIVersion string               `json:"apiVersion"`
	Kind       string               `json:"kind"`
	Metadata   kubernetesObjectMeta `json:"metadata"`
	Spec       kubernetesPVCSpec    `json:"spec"`
}

type kubernetesPVCSpec struct {
	AccessModes      []string               `json:"accessModes"`
	StorageClassName string                 `json:"storageClassName,omitempty"`
	Resources        kubernetesPVCResources `json:"resources"`
}

type kubernetesPVCResources struct {
	Requests map[string]string `json:"requests"`
}

type kubernetesJob struct {
	APIVersion string               `json:"apiVersion"`
	Kind       string               `json:"kind"`
	Metadata   kubernetesObjectMeta `json:"metadata"`
	Spec       kubernetesJobSpec    `json:"spec"`
}

type kubernetesJobSpec struct {
	BackoffLimit *int32                `json:"backoffLimit"`
	Template     kubernetesPodTemplate `json:"template"`
}

type kubernetesPodTemplate struct {
	Metadata kubernetesObjectMeta `json:"metadata"`
	Spec     kubernetesPodSpec    `json:"spec"`
}

type kubernetesPodSpec struct {
	RestartPolicy    string                           `json:"restartPolicy"`
	SecurityContext  *kubernetesPodSecurityContext    `json:"securityContext,omitempty"`
	ImagePullSecrets []kubernetesLocalObjectReference `json:"imagePullSecrets,omitempty"`
	Containers       []kubernetesContainer            `json:"containers"`
	Volumes          []kubernetesVolume               `json:"volumes"`
}

type kubernetesPodSecurityContext struct {
	RunAsUser    *int64 `json:"runAsUser,omitempty"`
	RunAsGroup   *int64 `json:"runAsGroup,omitempty"`
	RunAsNonRoot *bool  `json:"runAsNonRoot,omitempty"`
	FSGroup      *int64 `json:"fsGroup,omitempty"`
}

type kubernetesLocalObjectReference struct {
	Name string `json:"name"`
}

type kubernetesContainer struct {
	Name         string                  `json:"name"`
	Image        string                  `json:"image"`
	Command      []string                `json:"command"`
	Env          []voyagerEnvVar         `json:"env"`
	VolumeMounts []kubernetesVolumeMount `json:"volumeMounts"`
}

type kubernetesVolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
}

type kubernetesVolume struct {
	Name                  string                    `json:"name"`
	PersistentVolumeClaim kubernetesPVCVolumeSource `json:"persistentVolumeClaim"`
}

type kubernetesPVCVolumeSource struct {
	ClaimName string `json:"claimName"`
}
