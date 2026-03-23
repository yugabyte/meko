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

func newAccount(name, accountID string, tier saasv1.AccountTier) *saasv1.MekoUserAccount {
	return &saasv1.MekoUserAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
		},
		Spec: saasv1.MekoUserAccountSpec{
			AccountID:     accountID,
			AccountTier:   tier,
			MCPImage:      "mcp:" + string(tier),
			RAGImage:      "rag:" + string(tier),
			LangfuseImage: "langfuse:" + string(tier),
			MCPReplicas:   1,
			RAGReplicas:   1,
		},
	}
}

func reconcileOnce(ctx context.Context, r *MekoUserAccountReconciler, nn types.NamespacedName) {
	_, err := r.Reconcile(ctx, reconcile.Request{NamespacedName: nn})
	ExpectWithOffset(1, err).NotTo(HaveOccurred())
}

var _ = Describe("MekoUserAccount Controller", func() {
	var (
		reconciler *MekoUserAccountReconciler
		testCtx    context.Context
	)

	BeforeEach(func() {
		testCtx = context.Background()
		reconciler = &MekoUserAccountReconciler{
			Client: k8sClient,
			Scheme: k8sClient.Scheme(),
		}
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

		It("should create 3 Deployments with correct images and env vars", func() {
			reconcileOnce(testCtx, reconciler, nn)
			reconcileOnce(testCtx, reconciler, nn)

			tenantNS := TenantNamespace(accountID)

			for _, tc := range []struct {
				name  string
				image string
			}{
				{"mcp-server", "mcp:free"},
				{"rag-worker", "rag:free"},
				{"langfuse", "langfuse:free"},
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

		It("should create 2 ClusterIP Services (mcp-server, langfuse)", func() {
			reconcileOnce(testCtx, reconciler, nn)
			reconcileOnce(testCtx, reconciler, nn)

			tenantNS := TenantNamespace(accountID)

			for _, tc := range []struct {
				name string
				port int32
			}{
				{"mcp-server", 8080},
				{"langfuse", 3000},
			} {
				svc := &corev1.Service{}
				Expect(k8sClient.Get(testCtx, types.NamespacedName{
					Name: tc.name, Namespace: tenantNS,
				}, svc)).To(Succeed(), "service %s should exist", tc.name)

				Expect(svc.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
				Expect(svc.Spec.Ports).To(HaveLen(1))
				Expect(svc.Spec.Ports[0].Port).To(Equal(tc.port))
				Expect(svc.Spec.Selector).To(HaveKeyWithValue(LabelApp, tc.name))
				Expect(svc.Spec.Selector).To(HaveKeyWithValue(LabelAccountID, accountID))
			}

			// rag-worker should NOT have a service.
			ragSvc := &corev1.Service{}
			err := k8sClient.Get(testCtx, types.NamespacedName{
				Name: "rag-worker", Namespace: tenantNS,
			}, ragSvc)
			Expect(apierrors.IsNotFound(err)).To(BeTrue(), "rag-worker should have no service")
		})

		It("should set status to Active with namespace name", func() {
			reconcileOnce(testCtx, reconciler, nn)
			reconcileOnce(testCtx, reconciler, nn)

			account := &saasv1.MekoUserAccount{}
			Expect(k8sClient.Get(testCtx, nn, account)).To(Succeed())
			Expect(account.Status.Phase).To(Equal("Active"))
			Expect(account.Status.NamespaceName).To(Equal(TenantNamespace(accountID)))
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
			Expect(depList.Items).To(HaveLen(3))

			svcList := &corev1.ServiceList{}
			Expect(k8sClient.List(testCtx, svcList, &client.ListOptions{
				Namespace: tenantNS,
			})).To(Succeed())
			Expect(svcList.Items).To(HaveLen(2))
		})
	})
})
