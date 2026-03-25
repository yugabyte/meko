/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	saasv1 "github.com/meko/meko-provisioner/api/v1"
)

const (
	LabelApp       = "app"
	LabelAccountID = "account-id"

	RAGConfigMapName = "rag-agent-config"
	RAGSecretName    = "rag-agent-secret"
 
	MCPConfigMapName = "mcp-server-config"
	MCPSecretName    = "mcp-server-secret"
)

func TenantNamespace(accountID string) string {
	return "tenant-" + accountID
}

func commonLabels(app, accountID string) map[string]string {
	return map[string]string{
		LabelApp:       app,
		LabelAccountID: accountID,
	}
}

func accountEnv(account *saasv1.MekoUserAccount) []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "MEKO_ACCOUNT_ID", Value: account.Spec.AccountID},
		{Name: "MEKO_ACCOUNT_TIER", Value: string(account.Spec.AccountTier)},
	}
}

func secretEnvVar(envName, secretName, key string, optional bool) corev1.EnvVar {
	return corev1.EnvVar{
		Name: envName,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
				Optional:             ptr.To(optional),
			},
		},
	}
}

func BuildMCPConfigMap(account *saasv1.MekoUserAccount, ns string) *corev1.ConfigMap {
	labels := commonLabels("mcp-server", account.Spec.AccountID)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      MCPConfigMapName,
			Namespace: ns,
			Labels:    labels,
		},
		Data: map[string]string{
			"YB_MCP_TRANSPORT":      "http",
			"YB_LOG_LEVEL":          "DEBUG",
			"YB_ALLOWED_HOSTS":      "all",
			"YB_MCP_STATELESS_HTTP": "true",
		},
	}
}

func BuildMCPSecret(account *saasv1.MekoUserAccount, ns string, sharedData map[string][]byte) *corev1.Secret {
	labels := commonLabels("mcp-server", account.Spec.AccountID)
	data := map[string][]byte{
		"HOST":                 []byte(account.Spec.YugabyteDBHost),
		"YUGABYTEDB_PASSWORD":  []byte(account.Spec.YugabyteDBPassword),
		"PASSWORD":             []byte(account.Spec.YugabyteDBPassword),
		"YUGABYTEDB_URL":       []byte(account.Spec.YugabyteDBConnectionStringLibpq()),
	}
	if v, ok := sharedData["OPENAI_API_KEY"]; ok && len(v) > 0 {
		data["OPENAI_API_KEY"] = v
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      MCPSecretName,
			Namespace: ns,
			Labels:    labels,
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

func BuildMCPDeployment(account *saasv1.MekoUserAccount, ns string) *appsv1.Deployment {
	replicas := account.Spec.MCPReplicas
	if replicas == 0 {
		replicas = 1
	}
	labels := commonLabels("mcp-server", account.Spec.AccountID)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mcp-server",
			Namespace: ns,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false),
					Containers: []corev1.Container{{
						Name:            "mcp-server",
						Image:           account.Spec.MCPImage,
						ImagePullPolicy: corev1.PullAlways,
						Env: append(accountEnv(account),
							secretEnvVar("YUGABYTEDB_PASSWORD", MCPSecretName, "YUGABYTEDB_PASSWORD", false),
							secretEnvVar("OPENAI_API_KEY", MCPSecretName, "OPENAI_API_KEY", false),
							secretEnvVar("HOST", MCPSecretName, "HOST", false),
							secretEnvVar("PASSWORD", MCPSecretName, "PASSWORD", false),
							secretEnvVar("YUGABYTEDB_URL", MCPSecretName, "YUGABYTEDB_URL", false),
						),
						EnvFrom: []corev1.EnvFromSource{{
							ConfigMapRef: &corev1.ConfigMapEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: MCPConfigMapName},
							},
						}},
						Ports: []corev1.ContainerPort{{
							Name:          "http",
							ContainerPort: 8000,
							Protocol:      corev1.ProtocolTCP,
						}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("1Gi"),
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/ping",
									Port: intstr.FromInt32(8000),
								},
							},
							InitialDelaySeconds: 10,
							PeriodSeconds:       10,
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/ping",
									Port: intstr.FromInt32(8000),
								},
							},
							InitialDelaySeconds: 15,
							PeriodSeconds:       3600,
						},
					}},
					RestartPolicy: corev1.RestartPolicyAlways,
				},
			},
		},
	}
}

func BuildRAGConfigMap(account *saasv1.MekoUserAccount, ns string) *corev1.ConfigMap {
	labels := commonLabels("rag-worker", account.Spec.AccountID)
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RAGConfigMapName,
			Namespace: ns,
			Labels:    labels,
		},
		Data: map[string]string{
			"AWS_REGION":          "us-east-1",
			"AWS_S3_BUCKET_NAME":  "proto-automated-embedding",
			"TASK_LEASE_DURATION": "600",
		},
	}
}

func BuildRAGSecret(account *saasv1.MekoUserAccount, ns string, sharedData map[string][]byte) *corev1.Secret {
	labels := commonLabels("rag-worker", account.Spec.AccountID)
	data := map[string][]byte{
		"YUGABYTEDB_CONNECTION_STRING": []byte(account.Spec.YugabyteDBConnectionStringURI()),
	}
	for _, key := range []string{"OPENAI_API_KEY", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY"} {
		if v, ok := sharedData[key]; ok && len(v) > 0 {
			data[key] = v
		}
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RAGSecretName,
			Namespace: ns,
			Labels:    labels,
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

func BuildRAGDeployment(account *saasv1.MekoUserAccount, ns string) *appsv1.Deployment {
	replicas := account.Spec.RAGReplicas
	if replicas == 0 {
		replicas = 1
	}
	labels := commonLabels("rag-worker", account.Spec.AccountID)

	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rag-worker",
			Namespace: ns,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(replicas),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxSurge:       ptr.To(intstr.FromInt32(1)),
					MaxUnavailable: ptr.To(intstr.FromInt32(1)),
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: ptr.To(int64(120)),
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser:    ptr.To(int64(1001)),
						RunAsNonRoot: ptr.To(true),
						FSGroup:      ptr.To(int64(1001)),
					},
					AutomountServiceAccountToken: ptr.To(false),
					Containers: []corev1.Container{{
						Name:            "rag-worker",
						Image:           account.Spec.RAGImage,
						ImagePullPolicy: corev1.PullAlways,
						Env: append(accountEnv(account),
							secretEnvVar("YUGABYTEDB_CONNECTION_STRING", RAGSecretName, "YUGABYTEDB_CONNECTION_STRING", false),
							secretEnvVar("OPENAI_API_KEY", RAGSecretName, "OPENAI_API_KEY", false),
							secretEnvVar("AWS_ACCESS_KEY_ID", RAGSecretName, "AWS_ACCESS_KEY_ID", true),
							secretEnvVar("AWS_SECRET_ACCESS_KEY", RAGSecretName, "AWS_SECRET_ACCESS_KEY", true),
						),
						EnvFrom: []corev1.EnvFromSource{{
							ConfigMapRef: &corev1.ConfigMapEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: RAGConfigMapName},
							},
						}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("500m"),
								corev1.ResourceMemory: resource.MustParse("4Gi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("2"),
								corev1.ResourceMemory: resource.MustParse("8Gi"),
							},
						},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(false),
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
						LivenessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								Exec: &corev1.ExecAction{
									Command: []string{"/bin/sh", "-c", "find /app/rag_agent/app.log -mmin -5 | grep -q ."},
								},
							},
							InitialDelaySeconds: 60,
							PeriodSeconds:       60,
							FailureThreshold:    3,
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "tmp",
							MountPath: "/tmp",
						}},
					}},
					Volumes: []corev1.Volume{{
						Name: "tmp",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					}},
					RestartPolicy: corev1.RestartPolicyAlways,
				},
			},
		},
	}
}

func BuildLangfuseDeployment(account *saasv1.MekoUserAccount, ns string) *appsv1.Deployment {
	labels := commonLabels("langfuse", account.Spec.AccountID)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "langfuse",
			Namespace: ns,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false),
					Containers: []corev1.Container{{
						Name:  "langfuse",
						Image: account.Spec.LangfuseImage,
						Env:   accountEnv(account),
					}},
				},
			},
		},
	}
}

func BuildService(name, ns, accountID string, port int32) *corev1.Service {
	labels := commonLabels(name, accountID)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Port:       port,
				TargetPort: intstr.FromInt32(port),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}
}
