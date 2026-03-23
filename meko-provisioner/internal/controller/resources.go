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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	saasv1 "github.com/meko/meko-provisioner/api/v1"
)

const (
	LabelApp       = "app"
	LabelAccountID = "account-id"
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
						Name:  "mcp-server",
						Image: account.Spec.MCPImage,
						Env:   accountEnv(account),
					}},
				},
			},
		},
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
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken: ptr.To(false),
					Containers: []corev1.Container{{
						Name:  "rag-worker",
						Image: account.Spec.RAGImage,
						Env:   accountEnv(account),
					}},
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
