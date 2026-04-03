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
	"log/slog"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	saasv1 "github.com/meko/meko-provisioner/api/v1"
)

const (
	finalizerName = "saas.mekodata.ai/cleanup"

	SharedSecretName      = "meko-shared-credentials"
	SharedSecretNamespace = "meko-system"
)

// MekoUserAccountReconciler reconciles a MekoUserAccount object
type MekoUserAccountReconciler struct {
	client.Client
	Scheme           *runtime.Scheme
	MCPIngressDomain string
}

// +kubebuilder:rbac:groups=saas.mekodata.ai,resources=mekouseraccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=saas.mekodata.ai,resources=mekouseraccounts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=saas.mekodata.ai,resources=mekouseraccounts/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces;services,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete

func (r *MekoUserAccountReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	account := &saasv1.MekoUserAccount{}
	if err := r.Get(ctx, req.NamespacedName, account); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	accountID := account.Spec.AccountID
	ns := TenantNamespace(accountID)

	// Handle deletion via finalizer (Step 3 will fill this in).
	if !account.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, account, ns)
	}

	// Add finalizer if not present.
	if !controllerutil.ContainsFinalizer(account, finalizerName) {
		controllerutil.AddFinalizer(account, finalizerName)
		if err := r.Update(ctx, account); err != nil {
			return ctrl.Result{}, fmt.Errorf("adding finalizer: %w", err)
		}
		slog.Info("Added finalizer", "accountId", accountID)
	}

	// 1. Create namespace.
	if err := r.ensureNamespace(ctx, ns); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring namespace %s: %w", ns, err)
	}

	// 2. Read shared credentials and create RAG ConfigMap + Secret.
	sharedData, err := r.fetchSharedSecretData(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("reading shared secret %s/%s: %w", SharedSecretNamespace, SharedSecretName, err)
	}
	slog.Info("Fetched shared credentials", "accountId", accountID, "sourceNamespace", SharedSecretNamespace)

	if err := r.ensureConfigMap(ctx, BuildRAGConfigMap(account, ns)); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring configmap %s/%s: %w", ns, RAGConfigMapName, err)
	}
	if err := r.ensureSecret(ctx, BuildRAGSecret(account, ns, sharedData)); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring secret %s/%s: %w", ns, RAGSecretName, err)
	}

	if err := r.ensureConfigMap(ctx, BuildMCPConfigMap(account, ns)); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring configmap %s/%s: %w", ns, MCPConfigMapName, err)
	}
	if err := r.ensureSecret(ctx, BuildMCPSecret(account, ns, sharedData)); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring secret %s/%s: %w", ns, MCPSecretName, err)
	}

	// 3. Create deployments.
	deployments := []*appsv1.Deployment{
		BuildMCPDeployment(account, ns),
		BuildRAGDeployment(account, ns),
	}
	for _, dep := range deployments {
		if err := r.ensureDeployment(ctx, dep); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring deployment %s/%s: %w", dep.Namespace, dep.Name, err)
		}
	}

	// 4. Create services.
	services := []*corev1.Service{
		BuildService("mcp-server", ns, accountID, 8000),
	}
	for _, svc := range services {
		if err := r.ensureService(ctx, svc); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring service %s/%s: %w", svc.Namespace, svc.Name, err)
		}
	}

	// 5. Update status.
	account.Status.Phase = "Active"
	account.Status.NamespaceName = ns
	if r.MCPIngressDomain != "" {
		account.Status.MCPEndpoint = fmt.Sprintf("https://%s.%s", accountID, r.MCPIngressDomain)
	}
	if err := r.Status().Update(ctx, account); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}
	slog.Info("Reconciliation complete", "accountId", accountID, "namespace", ns, "phase", "Active")

	return ctrl.Result{}, nil
}

// reconcileDelete is a placeholder for the finalizer-based deletion in Step 3.
func (r *MekoUserAccountReconciler) reconcileDelete(
	ctx context.Context, account *saasv1.MekoUserAccount, ns string,
) (ctrl.Result, error) {
	accountID := account.Spec.AccountID
	slog.Info("Handling deletion", "accountId", accountID, "namespace", ns)

	// Set phase to Deleting.
	if account.Status.Phase != "Deleting" {
		account.Status.Phase = "Deleting"
		if err := r.Status().Update(ctx, account); err != nil {
			return ctrl.Result{}, fmt.Errorf("setting Deleting phase: %w", err)
		}
	}

	// Delete tenant namespace (cascading delete of all resources inside).
	nsObj := &corev1.Namespace{}
	err := r.Get(ctx, client.ObjectKey{Name: ns}, nsObj)
	if err == nil {
		if err := r.Delete(ctx, nsObj); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("deleting namespace %s: %w", ns, err)
		}
		slog.Info("Namespace delete issued, requeueing", "accountId", accountID, "namespace", ns)
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	if !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("checking namespace %s: %w", ns, err)
	}

	// Namespace gone — remove finalizer.
	controllerutil.RemoveFinalizer(account, finalizerName)
	if err := r.Update(ctx, account); err != nil {
		return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
	}
	slog.Info("Finalizer removed, deletion complete", "accountId", accountID)

	return ctrl.Result{}, nil
}

func (r *MekoUserAccountReconciler) ensureNamespace(ctx context.Context, name string) error {
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: name},
	}
	err := r.Get(ctx, client.ObjectKey{Name: name}, ns)
	if apierrors.IsNotFound(err) {
		slog.Info("Creating namespace", "namespace", name)
		return r.Create(ctx, ns)
	}
	return err
}

func (r *MekoUserAccountReconciler) ensureDeployment(ctx context.Context, desired *appsv1.Deployment) error {
	existing := &appsv1.Deployment{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		slog.Info("Creating Deployment", "name", desired.Name, "namespace", desired.Namespace)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Spec = desired.Spec
	return r.Update(ctx, existing)
}

func (r *MekoUserAccountReconciler) ensureService(ctx context.Context, desired *corev1.Service) error {
	existing := &corev1.Service{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		slog.Info("Creating Service", "name", desired.Name, "namespace", desired.Namespace)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Spec.Selector = desired.Spec.Selector
	existing.Spec.Ports = desired.Spec.Ports
	return r.Update(ctx, existing)
}

func (r *MekoUserAccountReconciler) fetchSharedSecretData(ctx context.Context) (map[string][]byte, error) {
	secret := &corev1.Secret{}
	key := client.ObjectKey{Name: SharedSecretName, Namespace: SharedSecretNamespace}
	if err := r.Get(ctx, key, secret); err != nil {
		return nil, err
	}
	return secret.Data, nil
}

func (r *MekoUserAccountReconciler) ensureConfigMap(ctx context.Context, desired *corev1.ConfigMap) error {
	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		slog.Info("Creating ConfigMap", "name", desired.Name, "namespace", desired.Namespace)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Data = desired.Data
	return r.Update(ctx, existing)
}

func (r *MekoUserAccountReconciler) ensureSecret(ctx context.Context, desired *corev1.Secret) error {
	existing := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		slog.Info("Creating Secret", "name", desired.Name, "namespace", desired.Namespace)
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	existing.Data = desired.Data
	return r.Update(ctx, existing)
}

// SetupWithManager sets up the controller with the Manager.
func (r *MekoUserAccountReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&saasv1.MekoUserAccount{}).
		Named("mekouseraccount").
		Complete(r)
}
