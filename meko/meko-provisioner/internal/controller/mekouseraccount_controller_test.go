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
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	saasv1 "github.com/meko/meko-provisioner/api/v1"
)

const (
	testDBHost            = "yb-tserver.yb-system.svc.cluster.local"
	testDBPort            = int32(5433)
	testDBUser            = "yugabyte"
	testDBPassword        = "testpass"
	testDBName            = "testdb"
	testLangfuseSecretKey = "sk-lf-test-secret"
	testLangfusePublicKey = "pk-lf-test-public"
	testLangfuseBaseURL   = "http://langfuse:3000"
)

func newAccount(name, accountID string, tier saasv1.AccountTier) *saasv1.MekoUserAccount {
	return &saasv1.MekoUserAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: saasv1.MekoUserAccountSpec{
			AccountID:          accountID,
			AccountTier:        tier,
			MCPImage:           "mcp:" + string(tier),
			RAGImage:           "rag:" + string(tier),
			YugabyteDBHost:     testDBHost,
			YugabyteDBPort:     testDBPort,
			YugabyteDBUser:     testDBUser,
			YugabyteDBPassword: testDBPassword,
			YugabyteDBName:     testDBName,
			MCPReplicas:        1,
			RAGReplicas:        1,
			LangfuseSecretKey:  testLangfuseSecretKey,
			LangfusePublicKey:  testLangfusePublicKey,
			LangfuseBaseURL:    testLangfuseBaseURL,
		},
	}
}

func reconcileOnce(ctx context.Context, r *MekoUserAccountReconciler, nn types.NamespacedName) {
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

func ensureTestSharedSecret(ctx context.Context, c client.Client) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: SharedSecretNamespace}}
	if err := c.Create(ctx, ns); err != nil && !apierrors.IsAlreadyExists(err) {
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SharedSecretName,
			Namespace: SharedSecretNamespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"OPENAI_API_KEY":        []byte("sk-test-key"),
			"AWS_ACCESS_KEY_ID":     []byte("AKIATEST"),
			"AWS_SECRET_ACCESS_KEY": []byte("test-secret"),
		},
	}
	if err := c.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		ExpectWithOffset(1, err).NotTo(HaveOccurred())
	}
}

var _ = Describe("MekoUserAccount Controller", func() {
	var (
		reconciler *MekoUserAccountReconciler
		testCtx    context.Context
	)

	BeforeEach(func() {
		testCtx = context.Background()
		reconciler = &MekoUserAccountReconciler{
			Client:           k8sClient,
			Scheme:           k8sClient.Scheme(),
			MCPIngressDomain: "mcp.mekodev.com",
		}
		ensureTestSharedSecret(testCtx, k8sClient)
	})

	Context("Provisioning a new account", func() {
		const (
			accountName = "test-user-prov"
			accountID   = "prov001"
		)
		var nn types.NamespacedName

		BeforeEach(func() {
			nn = types.NamespacedName{Name: accountName, Namespace: "default"}
			account := newAccount(accountName, accountID, saasv1.TierFree)
			Expect(k8sClient.Create(testCtx, account)).To(Succeed())
		})

		AfterEach(func() {
			account := &saasv1.MekoUserAccount{}
			if err := k8sClient.Get(testCtx, nn, account); err == nil {
				account.Finalizers = nil
				_ = k8sClient.Update(testCtx, account)
				_ = k8sClient.Delete(testCtx, account)
			}
			ns := &corev1.Namespace{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: TenantNamespace(accountID)}, ns); err == nil {
				_ = k8sClient.Delete(testCtx, ns)
			}
		})

		It("should create the tenant namespace", func() {
			reconcileOnce(testCtx, reconciler, nn)
			// Reconcile again after finalizer was added.
			reconcileOnce(testCtx, reconciler, nn)

			ns := &corev1.Namespace{}
			Expect(k8sClient.Get(testCtx, types.NamespacedName{Name: TenantNamespace(accountID)}, ns)).To(Succeed())
		})

		It("should create 2 Deployments with correct images and env vars", func() {
			reconcileOnce(testCtx, reconciler, nn)
			reconcileOnce(testCtx, reconciler, nn)

			tenantNS := TenantNamespace(accountID)

			for _, tc := range []struct {
				name  string
				image string
			}{
				{"mcp-server", "mcp:free"},
				{"rag-worker", "rag:free"},
			} {
				dep := &appsv1.Deployment{}
				Expect(k8sClient.Get(testCtx, types.NamespacedName{
					Name: tc.name, Namespace: tenantNS,
				}, dep)).To(Succeed(), "deployment %s should exist", tc.name)

				Expect(dep.Spec.Template.Spec.Containers).To(HaveLen(1))
				Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal(tc.image))

				envMap := map[string]string{}
				for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
					envMap[e.Name] = e.Value
				}
				Expect(envMap).To(HaveKeyWithValue("MEKO_ACCOUNT_ID", accountID))
				Expect(envMap).To(HaveKeyWithValue("MEKO_ACCOUNT_TIER", "free"))

				Expect(*dep.Spec.Template.Spec.AutomountServiceAccountToken).To(BeFalse())
			}
		})

		It("should create 1 ClusterIP Service (mcp-server)", func() {
			reconcileOnce(testCtx, reconciler, nn)
			reconcileOnce(testCtx, reconciler, nn)

			tenantNS := TenantNamespace(accountID)

			svc := &corev1.Service{}
			Expect(k8sClient.Get(testCtx, types.NamespacedName{
				Name: "mcp-server", Namespace: tenantNS,
			}, svc)).To(Succeed())

			Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
			Expect(svc.Spec.Ports).To(HaveLen(1))
			Expect(svc.Spec.Ports[0].Port).To(Equal(int32(8000)))
			Expect(svc.Spec.Selector).To(HaveKeyWithValue(LabelApp, "mcp-server"))
			Expect(svc.Spec.Selector).To(HaveKeyWithValue(LabelAccountID, accountID))

			// rag-worker and langfuse should NOT have services.
			for _, name := range []string{"rag-worker", "langfuse"} {
				noSvc := &corev1.Service{}
				err := k8sClient.Get(testCtx, types.NamespacedName{
					Name: name, Namespace: tenantNS,
				}, noSvc)
				Expect(apierrors.IsNotFound(err)).To(BeTrue(), "%s should have no service", name)
			}
		})

		It("should set status to Active with namespace name and MCP endpoint", func() {
			reconcileOnce(testCtx, reconciler, nn)
			reconcileOnce(testCtx, reconciler, nn)

			account := &saasv1.MekoUserAccount{}
			Expect(k8sClient.Get(testCtx, nn, account)).To(Succeed())
			Expect(account.Status.Phase).To(Equal("Active"))
			Expect(account.Status.NamespaceName).To(Equal(TenantNamespace(accountID)))
			Expect(account.Status.MCPEndpoint).To(Equal("https://" + accountID + ".mcp.mekodev.com"))
		})

		It("should leave MCPEndpoint empty when ingress domain is not configured", func() {
			noDomainReconciler := &MekoUserAccountReconciler{
				Client:           k8sClient,
				Scheme:           k8sClient.Scheme(),
				MCPIngressDomain: "",
			}
			reconcileOnce(testCtx, noDomainReconciler, nn)
			reconcileOnce(testCtx, noDomainReconciler, nn)

			account := &saasv1.MekoUserAccount{}
			Expect(k8sClient.Get(testCtx, nn, account)).To(Succeed())
			Expect(account.Status.Phase).To(Equal("Active"))
			Expect(account.Status.MCPEndpoint).To(BeEmpty())
		})

		It("should create the RAG ConfigMap with expected keys", func() {
			reconcileOnce(testCtx, reconciler, nn)
			reconcileOnce(testCtx, reconciler, nn)

			tenantNS := TenantNamespace(accountID)
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(testCtx, types.NamespacedName{
				Name: RAGConfigMapName, Namespace: tenantNS,
			}, cm)).To(Succeed())

			Expect(cm.Data).To(HaveKeyWithValue("AWS_REGION", "us-east-1"))
			Expect(cm.Data).To(HaveKeyWithValue("AWS_S3_BUCKET_NAME", "proto-automated-embedding"))
			Expect(cm.Data).To(HaveKeyWithValue("TASK_LEASE_DURATION", "600"))
			Expect(cm.Labels).To(HaveKeyWithValue(LabelApp, "rag-worker"))
			Expect(cm.Labels).To(HaveKeyWithValue(LabelAccountID, accountID))
		})

		It("should create the RAG Secret with merged credentials", func() {
			reconcileOnce(testCtx, reconciler, nn)
			reconcileOnce(testCtx, reconciler, nn)

			tenantNS := TenantNamespace(accountID)
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(testCtx, types.NamespacedName{
				Name: RAGSecretName, Namespace: tenantNS,
			}, secret)).To(Succeed())

			Expect(secret.Data).To(HaveKey("YUGABYTEDB_CONNECTION_STRING"))
			expectedURI := fmt.Sprintf("postgresql://%s:%s@%s:%d/%s",
				testDBUser, testDBPassword, testDBHost, testDBPort, testDBName)
			Expect(string(secret.Data["YUGABYTEDB_CONNECTION_STRING"])).To(Equal(expectedURI))
			Expect(secret.Data).To(HaveKey("OPENAI_API_KEY"))
			Expect(string(secret.Data["OPENAI_API_KEY"])).To(Equal("sk-test-key"))
			Expect(secret.Data).To(HaveKey("AWS_ACCESS_KEY_ID"))
			Expect(secret.Data).To(HaveKey("AWS_SECRET_ACCESS_KEY"))
		})

		It("should create the rag-worker Deployment with full production spec", func() {
			reconcileOnce(testCtx, reconciler, nn)
			reconcileOnce(testCtx, reconciler, nn)

			tenantNS := TenantNamespace(accountID)
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(testCtx, types.NamespacedName{
				Name: "rag-worker", Namespace: tenantNS,
			}, dep)).To(Succeed())

			podSpec := dep.Spec.Template.Spec
			container := podSpec.Containers[0]

			By("checking pod-level security context")
			Expect(podSpec.SecurityContext).NotTo(BeNil())
			Expect(*podSpec.SecurityContext.RunAsUser).To(Equal(int64(1001)))
			Expect(*podSpec.SecurityContext.RunAsNonRoot).To(BeTrue())
			Expect(*podSpec.SecurityContext.FSGroup).To(Equal(int64(1001)))

			By("checking termination grace period")
			Expect(*podSpec.TerminationGracePeriodSeconds).To(Equal(int64(120)))

			By("checking container security context")
			Expect(container.SecurityContext).NotTo(BeNil())
			Expect(*container.SecurityContext.AllowPrivilegeEscalation).To(BeFalse())
			Expect(container.SecurityContext.Capabilities.Drop).To(ContainElement(corev1.Capability("ALL")))

			By("checking image pull policy")
			Expect(container.ImagePullPolicy).To(Equal(corev1.PullAlways))

			By("checking resource requests and limits")
			Expect(container.Resources.Requests.Cpu().String()).To(Equal("500m"))
			Expect(container.Resources.Requests.Memory().String()).To(Equal("4Gi"))
			Expect(container.Resources.Limits.Cpu().String()).To(Equal("2"))
			Expect(container.Resources.Limits.Memory().String()).To(Equal("8Gi"))

			By("checking liveness probe")
			Expect(container.LivenessProbe).NotTo(BeNil())
			Expect(container.LivenessProbe.Exec).NotTo(BeNil())
			Expect(container.LivenessProbe.InitialDelaySeconds).To(Equal(int32(60)))

			By("checking envFrom references the ConfigMap")
			Expect(container.EnvFrom).To(HaveLen(1))
			Expect(container.EnvFrom[0].ConfigMapRef.Name).To(Equal(RAGConfigMapName))

			By("checking env has secret key refs")
			envNames := map[string]bool{}
			for _, e := range container.Env {
				envNames[e.Name] = true
			}
			Expect(envNames).To(HaveKey("MEKO_ACCOUNT_ID"))
			Expect(envNames).To(HaveKey("YUGABYTEDB_CONNECTION_STRING"))
			Expect(envNames).To(HaveKey("OPENAI_API_KEY"))
			Expect(envNames).To(HaveKey("AWS_ACCESS_KEY_ID"))
			Expect(envNames).To(HaveKey("AWS_SECRET_ACCESS_KEY"))

			By("checking volume mounts")
			Expect(container.VolumeMounts).To(HaveLen(1))
			Expect(container.VolumeMounts[0].MountPath).To(Equal("/tmp"))
			Expect(podSpec.Volumes).To(HaveLen(1))
			Expect(podSpec.Volumes[0].EmptyDir).NotTo(BeNil())
		})

		It("should create the MCP ConfigMap with global config", func() {
			reconcileOnce(testCtx, reconciler, nn)
			reconcileOnce(testCtx, reconciler, nn)

			tenantNS := TenantNamespace(accountID)
			cm := &corev1.ConfigMap{}
			Expect(k8sClient.Get(testCtx, types.NamespacedName{
				Name: MCPConfigMapName, Namespace: tenantNS,
			}, cm)).To(Succeed())

			Expect(cm.Data).To(HaveKeyWithValue("YB_MCP_TRANSPORT", "http"))
			Expect(cm.Data).To(HaveKeyWithValue("YB_LOG_LEVEL", "DEBUG"))
			Expect(cm.Data).To(HaveKeyWithValue("YB_ALLOWED_HOSTS", "all"))
			Expect(cm.Data).To(HaveKeyWithValue("YB_MCP_STATELESS_HTTP", "true"))
			Expect(cm.Data).To(HaveKeyWithValue("LANGFUSE_BASE_URL", testLangfuseBaseURL))
			Expect(cm.Data).To(HaveKeyWithValue("LANGFUSE_HOST", testLangfuseBaseURL))
			Expect(cm.Labels).To(HaveKeyWithValue(LabelApp, "mcp-server"))
		})

		It("should create the MCP Secret with per-tenant DB credentials", func() {
			reconcileOnce(testCtx, reconciler, nn)
			reconcileOnce(testCtx, reconciler, nn)

			tenantNS := TenantNamespace(accountID)
			secret := &corev1.Secret{}
			Expect(k8sClient.Get(testCtx, types.NamespacedName{
				Name: MCPSecretName, Namespace: tenantNS,
			}, secret)).To(Succeed())

			Expect(string(secret.Data["HOST"])).To(Equal(testDBHost))
			Expect(string(secret.Data["YUGABYTEDB_PASSWORD"])).To(Equal(testDBPassword))
			Expect(string(secret.Data["PASSWORD"])).To(Equal(testDBPassword))
			expectedLibpq := fmt.Sprintf("host=%s port=%d dbname=%s user=%s password=%s",
				testDBHost, testDBPort, testDBName, testDBUser, testDBPassword)
			Expect(string(secret.Data["YUGABYTEDB_URL"])).To(Equal(expectedLibpq))
			Expect(secret.Data).To(HaveKey("OPENAI_API_KEY"))
			Expect(string(secret.Data["OPENAI_API_KEY"])).To(Equal("sk-test-key"))
			Expect(string(secret.Data["LANGFUSE_SECRET_KEY"])).To(Equal(testLangfuseSecretKey))
			Expect(string(secret.Data["LANGFUSE_PUBLIC_KEY"])).To(Equal(testLangfusePublicKey))
		})

		It("should create the mcp-server Deployment with full production spec", func() {
			reconcileOnce(testCtx, reconciler, nn)
			reconcileOnce(testCtx, reconciler, nn)

			tenantNS := TenantNamespace(accountID)
			dep := &appsv1.Deployment{}
			Expect(k8sClient.Get(testCtx, types.NamespacedName{
				Name: "mcp-server", Namespace: tenantNS,
			}, dep)).To(Succeed())

			container := dep.Spec.Template.Spec.Containers[0]

			By("checking image pull policy")
			Expect(container.ImagePullPolicy).To(Equal(corev1.PullAlways))

			By("checking container port")
			Expect(container.Ports).To(HaveLen(1))
			Expect(container.Ports[0].ContainerPort).To(Equal(int32(8000)))

			By("checking resource requests and limits")
			Expect(container.Resources.Requests.Cpu().String()).To(Equal("500m"))
			Expect(container.Resources.Requests.Memory().String()).To(Equal("512Mi"))
			Expect(container.Resources.Limits.Cpu().String()).To(Equal("1"))
			Expect(container.Resources.Limits.Memory().String()).To(Equal("1Gi"))

			By("checking readiness probe")
			Expect(container.ReadinessProbe).NotTo(BeNil())
			Expect(container.ReadinessProbe.HTTPGet.Path).To(Equal("/ping"))
			Expect(container.ReadinessProbe.HTTPGet.Port.IntValue()).To(Equal(8000))

			By("checking liveness probe")
			Expect(container.LivenessProbe).NotTo(BeNil())
			Expect(container.LivenessProbe.HTTPGet.Path).To(Equal("/ping"))
			Expect(container.LivenessProbe.PeriodSeconds).To(Equal(int32(3600)))

			By("checking envFrom references the ConfigMap")
			Expect(container.EnvFrom).To(HaveLen(1))
			Expect(container.EnvFrom[0].ConfigMapRef.Name).To(Equal(MCPConfigMapName))

			By("checking env has secret key refs for DB credentials")
			envNames := map[string]bool{}
			for _, e := range container.Env {
				envNames[e.Name] = true
			}
			Expect(envNames).To(HaveKey("MEKO_ACCOUNT_ID"))
			Expect(envNames).To(HaveKey("YUGABYTEDB_PASSWORD"))
			Expect(envNames).To(HaveKey("OPENAI_API_KEY"))
			Expect(envNames).To(HaveKey("HOST"))
			Expect(envNames).To(HaveKey("PASSWORD"))
			Expect(envNames).To(HaveKey("YUGABYTEDB_URL"))
			Expect(envNames).To(HaveKey("LANGFUSE_SECRET_KEY"))
			Expect(envNames).To(HaveKey("LANGFUSE_PUBLIC_KEY"))
		})
	})

	Context("Per-tier image variation", func() {
		tiers := []saasv1.AccountTier{saasv1.TierFree, saasv1.TierPro, saasv1.TierEnterprise}

		for _, tier := range tiers {
			It(fmt.Sprintf("should provision with %s tier images", tier), func() {
				name := fmt.Sprintf("tier-test-%s", tier)
				aid := fmt.Sprintf("tier%s", tier)
				nn := types.NamespacedName{Name: name, Namespace: "default"}
				account := newAccount(name, aid, tier)

				Expect(k8sClient.Create(testCtx, account)).To(Succeed())
				defer func() {
					a := &saasv1.MekoUserAccount{}
					if err := k8sClient.Get(testCtx, nn, a); err == nil {
						a.Finalizers = nil
						_ = k8sClient.Update(testCtx, a)
						_ = k8sClient.Delete(testCtx, a)
					}
					ns := &corev1.Namespace{}
					if err := k8sClient.Get(testCtx, types.NamespacedName{Name: TenantNamespace(aid)}, ns); err == nil {
						_ = k8sClient.Delete(testCtx, ns)
					}
				}()

				reconcileOnce(testCtx, reconciler, nn)
				reconcileOnce(testCtx, reconciler, nn)

				tenantNS := TenantNamespace(aid)
				dep := &appsv1.Deployment{}
				Expect(k8sClient.Get(testCtx, types.NamespacedName{
					Name: "mcp-server", Namespace: tenantNS,
				}, dep)).To(Succeed())
				Expect(dep.Spec.Template.Spec.Containers[0].Image).To(Equal("mcp:" + string(tier)))
			})
		}
	})

	Context("Idempotency", func() {
		const (
			accountName = "test-user-idem"
			accountID   = "idem001"
		)
		var nn types.NamespacedName

		BeforeEach(func() {
			nn = types.NamespacedName{Name: accountName, Namespace: "default"}
			account := newAccount(accountName, accountID, saasv1.TierFree)
			Expect(k8sClient.Create(testCtx, account)).To(Succeed())
		})

		AfterEach(func() {
			account := &saasv1.MekoUserAccount{}
			if err := k8sClient.Get(testCtx, nn, account); err == nil {
				account.Finalizers = nil
				_ = k8sClient.Update(testCtx, account)
				_ = k8sClient.Delete(testCtx, account)
			}
			ns := &corev1.Namespace{}
			if err := k8sClient.Get(testCtx, types.NamespacedName{Name: TenantNamespace(accountID)}, ns); err == nil {
				_ = k8sClient.Delete(testCtx, ns)
			}
		})

		It("should not duplicate resources when reconciled twice", func() {
			// First full reconcile (finalizer add + resource creation).
			reconcileOnce(testCtx, reconciler, nn)
			reconcileOnce(testCtx, reconciler, nn)

			// Second full reconcile — should be a no-op update.
			reconcileOnce(testCtx, reconciler, nn)

			tenantNS := TenantNamespace(accountID)
			depList := &appsv1.DeploymentList{}
			Expect(k8sClient.List(testCtx, depList, &client.ListOptions{
				Namespace: tenantNS,
			})).To(Succeed())
			Expect(depList.Items).To(HaveLen(2))

			svcList := &corev1.ServiceList{}
			Expect(k8sClient.List(testCtx, svcList, &client.ListOptions{
				Namespace: tenantNS,
			})).To(Succeed())
			Expect(svcList.Items).To(HaveLen(1))

			cmList := &corev1.ConfigMapList{}
			Expect(k8sClient.List(testCtx, cmList, &client.ListOptions{
				Namespace: tenantNS,
			})).To(Succeed())
			Expect(cmList.Items).To(HaveLen(2))

			secretList := &corev1.SecretList{}
			Expect(k8sClient.List(testCtx, secretList, &client.ListOptions{
				Namespace: tenantNS,
			})).To(Succeed())
			Expect(secretList.Items).To(HaveLen(2))
		})
	})
})
