package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

const (
	// notReadyRequeue bounds recovery time while workspace dependencies are
	// unavailable.
	notReadyRequeue = 30 * time.Second
	// readyRequeue lets the controller repair a deleted workspace LocalQueue
	// without introducing a hard startup dependency on the Kueue CRDs.
	readyRequeue = 5 * time.Minute
)

var (
	localQueueGVK   = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "LocalQueue"}
	clusterQueueGVK = schema.GroupVersionKind{Group: "kueue.x-k8s.io", Version: "v1beta2", Kind: "ClusterQueue"}
)

type TauWorkspaceReconciler struct {
	client.Client
	APIReader         client.Reader
	PlatformNamespace string
}

// +kubebuilder:rbac:groups=tau.azure.com,resources=workspaces,verbs=get;list;watch
// +kubebuilder:rbac:groups=tau.azure.com,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=tau.azure.com,resources=workspaces,verbs=update;patch
// +kubebuilder:rbac:groups=tau.azure.com,resources=workspaces/status,verbs=get;update;patch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=roles,verbs=get;list;watch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=localqueues,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=clusterqueues,verbs=get
// +kubebuilder:rbac:groups=kueue.x-k8s.io,resources=workloads,verbs=get;list;watch

func (r *TauWorkspaceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("tauworkspace", req.NamespacedName.String())

	var workspace tauv1alpha1.TauWorkspace
	if err := r.Get(ctx, req.NamespacedName, &workspace); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if workspace.Namespace != r.platformNamespace() {
		logger.Info("ignoring workspace outside platform namespace", "platformNamespace", r.platformNamespace())
		return ctrl.Result{}, nil
	}
	if !workspace.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&workspace, workspaceFinalizer) {
			if err := r.cleanupWorkspaceAccess(ctx, workspace.Name); err != nil {
				return ctrl.Result{}, err
			}
			controllerutil.RemoveFinalizer(&workspace, workspaceFinalizer)
			if err := r.Update(ctx, &workspace); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}
	if !controllerutil.ContainsFinalizer(&workspace, workspaceFinalizer) {
		controllerutil.AddFinalizer(&workspace, workspaceFinalizer)
		if err := r.Update(ctx, &workspace); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	primaryWorkspace, err := r.primaryWorkspace(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve v0 primary workspace: %w", err)
	}
	if primaryWorkspace != workspace.Name {
		if err := r.cleanupWorkspaceAccess(ctx, workspace.Name); err != nil {
			return ctrl.Result{}, err
		}
		message := fmt.Sprintf(
			"TauGrid v0 activates one workspace; %q is primary and %q is blocked",
			primaryWorkspace,
			workspace.Name,
		)
		conditions := []metav1.Condition{
			condition(tauv1alpha1.ConditionRBACReady, metav1.ConditionFalse, "AdditionalWorkspaceBlocked", message, workspace.Generation),
			condition(tauv1alpha1.ConditionQueueReady, metav1.ConditionFalse, "AdditionalWorkspaceBlocked", message, workspace.Generation),
			condition(tauv1alpha1.ConditionDriftDetected, metav1.ConditionFalse, "NoDrift", "No owned resource drift detected", workspace.Generation),
		}
		desiredStatus := tauv1alpha1.TauWorkspaceStatus{
			Phase:              workspacePhase(conditions),
			ObservedGeneration: workspace.Generation,
			Target: tauv1alpha1.WorkspaceTargetStatus{
				ResolvedNamespace: resolvedNamespace(&workspace),
			},
			Conditions: mergeConditions(workspace.Status.Conditions, conditions),
		}
		if !equalWorkspaceStatus(workspace.Status, desiredStatus) {
			workspace.Status = desiredStatus
			if err := r.Status().Update(ctx, &workspace); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: notReadyRequeue}, nil
	}
	if workspace.Annotations[annotationV0Primary] != "true" {
		before := workspace.DeepCopy()
		if workspace.Annotations == nil {
			workspace.Annotations = map[string]string{}
		}
		workspace.Annotations[annotationV0Primary] = "true"
		if err := r.Patch(ctx, &workspace, client.MergeFrom(before)); err != nil {
			return ctrl.Result{}, fmt.Errorf("persist v0 primary workspace marker: %w", err)
		}
		return ctrl.Result{Requeue: true}, nil
	}
	// spec.queue is optional so a platform can declare the queue name once on
	// the TauCluster singleton. Resolve it in memory before anything reads it;
	// status.queue.localQueue then reports the effective queue to the CLI.
	if strings.TrimSpace(workspace.Spec.Queue) == "" {
		resolved, err := r.defaultWorkspaceQueue(ctx)
		if err != nil {
			return r.reportUnresolvedQueue(ctx, &workspace, err.Error())
		}
		workspace.Spec.Queue = resolved
	}

	targetNamespace := resolvedNamespace(&workspace)
	conditions := []metav1.Condition{}

	namespaceReady, namespaceErr := r.reconcileNamespace(ctx, &workspace, targetNamespace)
	if namespaceErr != nil {
		namespaceReady = false
		conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "NamespaceReconcileFailed", namespaceErr.Error(), workspace.Generation))
	} else if namespaceReady {
		conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, false, "NoDrift", "target namespace is reconciled", workspace.Generation))
	}
	if namespaceReady {
		previousNamespace := workspace.Status.Target.ResolvedNamespace
		if previousNamespace != "" && previousNamespace != targetNamespace {
			if err := r.cleanupStaleNamespaceMetadata(ctx, workspace.Name, previousNamespace); err != nil {
				conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "NamespaceCleanupFailed", err.Error(), workspace.Generation))
			}
		}
	}

	rbacReady := false
	rbacMessage := "waiting for target namespace reconciliation"
	var rbacErr error
	if namespaceReady {
		rbacReady, rbacMessage, rbacErr = r.reconcileRBAC(ctx, &workspace, targetNamespace)
		if rbacErr != nil {
			rbacMessage = rbacErr.Error()
		}
	} else if namespaceErr != nil {
		rbacMessage = namespaceErr.Error()
	}
	rbacReadyReason := "RoleBindingReady"
	if authorizationMode(&workspace) == tauv1alpha1.AuthorizationModeClusterWide {
		rbacReadyReason = "ExistingClusterAuthorization"
	}
	conditions = append(conditions, boolCondition(tauv1alpha1.ConditionRBACReady, rbacReady && rbacErr == nil, reasonFor(rbacReady && rbacErr == nil, rbacReadyReason, "AuthorizationNotReady"), rbacMessage, workspace.Generation))
	queueStatus := tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue}
	queueReady := false
	queueMessage := "waiting for target namespace reconciliation"
	if namespaceReady {
		queueStatus, queueReady, queueMessage = r.reconcileQueue(ctx, &workspace, targetNamespace)
	} else if namespaceErr != nil {
		queueMessage = namespaceErr.Error()
	}
	conditions = append(conditions, boolCondition(tauv1alpha1.ConditionQueueReady, queueReady, reasonFor(queueReady, "QueueReady", "QueueNotReady"), queueMessage, workspace.Generation))
	queueNamespace, queueName := "", ""
	if namespaceReady {
		queueNamespace, queueName = targetNamespace, workspace.Spec.Queue
	}
	if err := r.cleanupStaleWorkspaceLocalQueues(ctx, workspace.Name, queueNamespace, queueName); err != nil {
		conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "QueueCleanupFailed", err.Error(), workspace.Generation))
	}

	workloadIdentityReady := false
	workloadIdentityMessage := "waiting for target namespace reconciliation"
	var workloadIdentityErr error
	if namespaceReady {
		workloadIdentityReady, workloadIdentityMessage, workloadIdentityErr = r.reconcileWorkloadIdentity(ctx, &workspace, targetNamespace)
		if workloadIdentityErr != nil {
			workloadIdentityMessage = workloadIdentityErr.Error()
		}
	} else if namespaceErr != nil {
		workloadIdentityMessage = namespaceErr.Error()
	}
	workloadIdentityStatus := metav1.ConditionUnknown
	workloadIdentityReason := "NotConfigured"
	if workspace.Spec.WorkloadIdentity != nil {
		workloadIdentityStatus = metav1.ConditionFalse
		workloadIdentityReason = "ServiceAccountNotReady"
		if workloadIdentityReady && workloadIdentityErr == nil {
			workloadIdentityStatus = metav1.ConditionTrue
			workloadIdentityReason = "ServiceAccountReady"
		}
	}
	conditions = append(conditions, condition(tauv1alpha1.ConditionWorkloadIdentityReady, workloadIdentityStatus, workloadIdentityReason, workloadIdentityMessage, workspace.Generation))

	keepNamespace, keepServiceAccount := "", ""
	keepResearcherBinding := false
	if namespaceReady {
		keepNamespace = targetNamespace
		keepResearcherBinding = authorizationMode(&workspace) != tauv1alpha1.AuthorizationModeClusterWide
		if workspace.Spec.WorkloadIdentity != nil {
			keepServiceAccount = workspace.Spec.WorkloadIdentity.ServiceAccountName
		}
	}
	if err := r.cleanupStaleTargetRBAC(ctx, workspace.Name, keepNamespace, keepServiceAccount, keepResearcherBinding); err != nil {
		conditions = append(conditions, boolCondition(tauv1alpha1.ConditionDriftDetected, true, "RBACCleanupFailed", err.Error(), workspace.Generation))
	}

	desired := tauv1alpha1.TauWorkspaceStatus{
		Phase:              workspacePhase(conditions),
		ObservedGeneration: workspace.Generation,
		Target:             tauv1alpha1.WorkspaceTargetStatus{ResolvedNamespace: targetNamespace},
		Queue:              queueStatus,
		Conditions:         mergeConditions(workspace.Status.Conditions, conditions),
	}
	if !equalWorkspaceStatus(workspace.Status, desired) {
		workspace.Status = desired
		if err := r.Status().Update(ctx, &workspace); err != nil {
			return ctrl.Result{}, err
		}
	}
	if desired.Phase == tauv1alpha1.WorkspacePhaseReady {
		return ctrl.Result{RequeueAfter: readyRequeue}, nil
	}
	return ctrl.Result{RequeueAfter: notReadyRequeue}, nil
}

func (r *TauWorkspaceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&tauv1alpha1.TauWorkspace{}).
		Complete(r)
}

func (r *TauWorkspaceReconciler) platformNamespace() string {
	if r.PlatformNamespace == "" {
		return tauv1alpha1.PlatformNamespace
	}
	return r.PlatformNamespace
}

// defaultWorkspaceQueue reads the cluster-wide workspace queue default from the
// TauCluster singleton. It is the same name the TauGrid distribution gives its
// baseline ClusterQueue, so a workspace that omits spec.queue still lands on a
// reviewed queue instead of guessing.
func (r *TauWorkspaceReconciler) defaultWorkspaceQueue(ctx context.Context) (string, error) {
	var cluster tauv1alpha1.TauCluster
	if err := r.Get(ctx, client.ObjectKey{Name: tauv1alpha1.TauClusterSingletonName}, &cluster); err != nil {
		return "", fmt.Errorf("spec.queue is empty and the TauCluster default is unavailable: %w", err)
	}
	queue := strings.TrimSpace(cluster.Spec.WorkspaceDefaults.DefaultQueue)
	if queue == "" {
		return "", fmt.Errorf("spec.queue is empty and TauCluster %q declares no workspaceDefaults.defaultQueue", cluster.Name)
	}
	return queue, nil
}

// reportUnresolvedQueue keeps an unresolvable workspace visible and Degraded
// rather than reconciling namespace, RBAC, or identity against a guessed queue.
func (r *TauWorkspaceReconciler) reportUnresolvedQueue(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, message string) (ctrl.Result, error) {
	conditions := []metav1.Condition{
		boolCondition(tauv1alpha1.ConditionRBACReady, false, "QueueUnresolved", message, workspace.Generation),
		boolCondition(tauv1alpha1.ConditionQueueReady, false, "QueueUnresolved", message, workspace.Generation),
		condition(tauv1alpha1.ConditionWorkloadIdentityReady, metav1.ConditionUnknown, "QueueUnresolved", message, workspace.Generation),
		boolCondition(tauv1alpha1.ConditionDriftDetected, false, "NoDrift", message, workspace.Generation),
	}
	desired := tauv1alpha1.TauWorkspaceStatus{
		Phase:              tauv1alpha1.WorkspacePhaseDegraded,
		ObservedGeneration: workspace.Generation,
		Target:             tauv1alpha1.WorkspaceTargetStatus{ResolvedNamespace: resolvedNamespace(workspace)},
		Conditions:         mergeConditions(workspace.Status.Conditions, conditions),
	}
	if !equalWorkspaceStatus(workspace.Status, desired) {
		workspace.Status = desired
		if err := r.Status().Update(ctx, workspace); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: notReadyRequeue}, nil
}

func resolvedNamespace(workspace *tauv1alpha1.TauWorkspace) string {
	if workspace.Spec.Target.Namespace != "" {
		return workspace.Spec.Target.Namespace
	}
	return workspace.Name
}

func (r *TauWorkspaceReconciler) primaryWorkspace(ctx context.Context) (string, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var workspaces tauv1alpha1.TauWorkspaceList
	if err := reader.List(ctx, &workspaces, client.InNamespace(r.platformNamespace())); err != nil {
		return "", err
	}
	var primary *tauv1alpha1.TauWorkspace
	var incumbent *tauv1alpha1.TauWorkspace
	var marked *tauv1alpha1.TauWorkspace
	for i := range workspaces.Items {
		candidate := &workspaces.Items[i]
		if candidate.Annotations[annotationV0Primary] == "true" {
			if marked != nil {
				return "", fmt.Errorf(
					"multiple TauWorkspaces claim the v0 primary marker: %q and %q",
					marked.Name,
					candidate.Name,
				)
			}
			marked = candidate
		}
		if hasPrimaryWorkspaceHistory(candidate) && (incumbent == nil || workspacePrecedes(candidate, incumbent)) {
			incumbent = candidate
		}
		if !candidate.DeletionTimestamp.IsZero() {
			continue
		}
		if primary == nil || workspacePrecedes(candidate, primary) {
			primary = candidate
		}
	}
	if marked != nil {
		return marked.Name, nil
	}
	if incumbent != nil {
		return incumbent.Name, nil
	}
	for i := range workspaces.Items {
		candidate := &workspaces.Items[i]
		hasAccess, err := r.hasWorkspaceDerivedState(ctx, candidate)
		if err != nil {
			return "", err
		}
		if hasAccess && (incumbent == nil || workspacePrecedes(candidate, incumbent)) {
			incumbent = candidate
		}
	}
	if incumbent != nil {
		return incumbent.Name, nil
	}
	if primary == nil {
		return "", fmt.Errorf("no non-terminating TauWorkspace exists")
	}
	return primary.Name, nil
}

func workspacePrecedes(a, b *tauv1alpha1.TauWorkspace) bool {
	return a.CreationTimestamp.Before(&b.CreationTimestamp) ||
		(a.CreationTimestamp.Equal(&b.CreationTimestamp) && a.Name < b.Name)
}

func hasPrimaryWorkspaceHistory(workspace *tauv1alpha1.TauWorkspace) bool {
	if workspace.Status.Phase == "" && workspace.Status.ObservedGeneration == 0 && len(workspace.Status.Conditions) == 0 {
		return false
	}
	for _, condition := range workspace.Status.Conditions {
		if condition.Reason == "AdditionalWorkspaceBlocked" {
			return false
		}
	}
	return true
}

func (r *TauWorkspaceReconciler) hasWorkspaceDerivedState(ctx context.Context, workspace *tauv1alpha1.TauWorkspace) (bool, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	targetNamespace := resolvedNamespace(workspace)
	var namespace corev1.Namespace
	if err := reader.Get(ctx, client.ObjectKey{Name: targetNamespace}, &namespace); err == nil {
		if namespace.Labels[labelWorkspace] == workspace.Name {
			return true, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	var binding rbacv1.RoleBinding
	if err := reader.Get(ctx, client.ObjectKey{Name: defaultRoleName, Namespace: targetNamespace}, &binding); err == nil {
		if binding.Labels[labelManagedBy] == labelManagedByValue && binding.Labels[labelWorkspace] == workspace.Name {
			return true, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	localQueue := &unstructured.Unstructured{}
	localQueue.SetGroupVersionKind(localQueueGVK)
	if err := reader.Get(ctx, client.ObjectKey{Name: workspace.Spec.Queue, Namespace: targetNamespace}, localQueue); err == nil {
		if localQueue.GetLabels()[labelManagedBy] == labelManagedByValue &&
			localQueue.GetLabels()[labelWorkspace] == workspace.Name {
			return true, nil
		}
	} else if !apierrors.IsNotFound(err) {
		return false, err
	}
	return false, nil
}

func (r *TauWorkspaceReconciler) reconcileNamespace(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, targetNamespace string) (bool, error) {
	if reason := reservedNamespaceReason(targetNamespace, r.platformNamespace()); reason != "" {
		return false, fmt.Errorf("refusing to manage namespace %q: %s", targetNamespace, reason)
	}
	var namespace corev1.Namespace
	err := r.Get(ctx, client.ObjectKey{Name: targetNamespace}, &namespace)
	if apierrors.IsNotFound(err) && workspace.Spec.Target.CreateNamespace {
		namespace = corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name:        targetNamespace,
				Labels:      workspaceNamespaceLabels(workspace.Name, workspace.Spec.Queue),
				Annotations: workspaceNamespaceAnnotations(workspace.Spec.Defaults.OutputRoot),
			},
		}
		if err := r.Create(ctx, &namespace); err != nil {
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if namespace.Labels == nil {
		namespace.Labels = map[string]string{}
	}
	if owner := namespace.Labels[labelWorkspace]; owner != "" && owner != workspace.Name {
		orphaned, err := r.ownerWorkspaceAbsent(ctx, owner)
		if err != nil {
			return false, err
		}
		if !orphaned {
			return false, fmt.Errorf("target namespace %q is already assigned to TauWorkspace %q", targetNamespace, owner)
		}
		log.FromContext(ctx).Info("reclaiming namespace from deleted workspace",
			"namespace", targetNamespace, "previousOwner", owner, "workspace", workspace.Name)
	}
	changed := false
	requiredLabels := map[string]string{
		labelWorkspace:              workspace.Name,
		labelWorkspaceLocalQueue:    workspace.Spec.Queue,
		labelKueueDefaultLocalQueue: workspace.Spec.Queue,
	}
	if workspace.Spec.Target.CreateNamespace {
		for k, v := range workspaceLabels(workspace.Name) {
			requiredLabels[k] = v
		}
		requiredLabels[labelPSAEnforce] = "baseline"
		requiredLabels[labelPSAAudit] = "restricted"
		requiredLabels[labelPSAWarn] = "restricted"
	}
	for k, v := range requiredLabels {
		if namespace.Labels[k] != v {
			namespace.Labels[k] = v
			changed = true
		}
	}
	if namespace.Annotations == nil {
		namespace.Annotations = map[string]string{}
	}
	resultScope := strings.TrimSpace(workspace.Spec.Defaults.OutputRoot)
	if resultScope == "" {
		if _, ok := namespace.Annotations[annotationResultScope]; ok {
			delete(namespace.Annotations, annotationResultScope)
			changed = true
		}
	} else if namespace.Annotations[annotationResultScope] != resultScope {
		namespace.Annotations[annotationResultScope] = resultScope
		changed = true
	}
	if changed {
		if err := r.Update(ctx, &namespace); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (r *TauWorkspaceReconciler) reconcileRBAC(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, targetNamespace string) (bool, string, error) {
	if authorizationMode(workspace) == tauv1alpha1.AuthorizationModeClusterWide {
		if err := r.cleanupResearcherRBAC(ctx, workspace.Name, targetNamespace); err != nil {
			return false, "failed to remove subject-specific researcher RBAC for cluster-wide authorization", err
		}
		if err := r.cleanupPlatformReaderRBAC(ctx, workspace.Name); err != nil {
			return false, "failed to remove subject-specific workspace reader RBAC for cluster-wide authorization", err
		}
		return true, "workspace relies on pre-existing cluster authorization; the controller grants no researcher access", nil
	}
	if workspace.Spec.PrincipalRef == nil || workspace.Spec.KubernetesSubject == nil {
		return false, "workspace-rbac authorization requires principalRef and kubernetesSubject", nil
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: defaultRoleName, Namespace: targetNamespace}}
	if err := r.requireWorkspaceOwnershipOrAbsence(ctx, binding, workspace.Name); err != nil {
		return false, "refusing to adopt existing researcher RoleBinding", err
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.Labels = workspaceLabels(workspace.Name)
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: defaultRoleName}
		binding.Subjects = []rbacv1.Subject{rbacSubject(*workspace.Spec.KubernetesSubject, targetNamespace)}
		return nil
	})
	if err != nil {
		return false, "failed to reconcile researcher RoleBinding", err
	}
	if err := r.reconcilePlatformReaderRBAC(ctx, workspace); err != nil {
		return false, "failed to reconcile workspace reader RoleBinding", err
	}
	return true, "researcher ClusterRole binding is reconciled", nil
}

func authorizationMode(workspace *tauv1alpha1.TauWorkspace) string {
	if workspace.Spec.Authorization == nil || workspace.Spec.Authorization.Mode == "" {
		return tauv1alpha1.AuthorizationModeWorkspaceRBAC
	}
	return workspace.Spec.Authorization.Mode
}

func (r *TauWorkspaceReconciler) cleanupResearcherRBAC(ctx context.Context, workspaceName, targetNamespace string) error {
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: defaultRoleName, Namespace: targetNamespace}}
	return r.deleteOwnedObject(ctx, binding, workspaceName)
}

func (r *TauWorkspaceReconciler) reconcileWorkloadIdentity(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, targetNamespace string) (bool, string, error) {
	if workspace.Spec.WorkloadIdentity == nil {
		return false, "workspace does not configure workload identity defaults", nil
	}
	wi := workspace.Spec.WorkloadIdentity
	serviceAccount := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: wi.ServiceAccountName, Namespace: targetNamespace}}
	if err := r.requireWorkspaceOwnershipOrAbsence(ctx, serviceAccount, workspace.Name); err != nil {
		return false, "refusing to adopt existing workload identity ServiceAccount", err
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, serviceAccount, func() error {
		if serviceAccount.Labels == nil {
			serviceAccount.Labels = map[string]string{}
		}
		for k, v := range workspaceLabels(workspace.Name) {
			serviceAccount.Labels[k] = v
		}
		serviceAccount.Labels[labelAzureWIUse] = "true"
		if serviceAccount.Annotations == nil {
			serviceAccount.Annotations = map[string]string{}
		}
		serviceAccount.Annotations[annotationAzureWIClientID] = wi.ClientID
		return nil
	})
	if err != nil {
		return false, "failed to reconcile workload identity ServiceAccount", err
	}
	return true, "workload identity ServiceAccount is reconciled", nil
}

func (r *TauWorkspaceReconciler) reconcilePlatformReaderRBAC(ctx context.Context, workspace *tauv1alpha1.TauWorkspace) error {
	name := "tau-workspace-reader-" + workspace.Name
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.platformNamespace()}}
	if err := r.requireWorkspaceOwnershipOrAbsence(ctx, role, workspace.Name); err != nil {
		return err
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Labels = workspaceLabels(workspace.Name)
		role.Rules = []rbacv1.PolicyRule{
			{APIGroups: []string{"tau.azure.com"}, Resources: []string{"workspaces", "workspaces/status"}, ResourceNames: []string{workspace.Name}, Verbs: []string{"get"}},
			{APIGroups: []string{"tau.azure.com"}, Resources: []string{"quotarequests"}, Verbs: []string{"create", "get"}},
		}
		return nil
	})
	if err != nil {
		return err
	}
	binding := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.platformNamespace()}}
	if err := r.requireWorkspaceOwnershipOrAbsence(ctx, binding, workspace.Name); err != nil {
		return err
	}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, binding, func() error {
		binding.Labels = workspaceLabels(workspace.Name)
		binding.RoleRef = rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: name}
		binding.Subjects = []rbacv1.Subject{rbacSubject(*workspace.Spec.KubernetesSubject, r.platformNamespace())}
		return nil
	})
	return err
}

func rbacSubject(subject tauv1alpha1.KubernetesSubject, serviceAccountNamespace string) rbacv1.Subject {
	out := rbacv1.Subject{Kind: subject.Kind, Name: subject.Name}
	switch subject.Kind {
	case rbacv1.UserKind, rbacv1.GroupKind:
		out.APIGroup = rbacv1.GroupName
	case rbacv1.ServiceAccountKind:
		out.Namespace = serviceAccountNamespace
	}
	return out
}

// reconcileQueue accepts an existing platform-owned LocalQueue or creates the
// workspace LocalQueue when a same-named ClusterQueue exists. The latter is the
// portable TauGrid bootstrap contract: Helm owns the ClusterQueue, while this
// controller owns state that depends on a future workspace namespace.
func (r *TauWorkspaceReconciler) reconcileQueue(ctx context.Context, workspace *tauv1alpha1.TauWorkspace, targetNamespace string) (tauv1alpha1.WorkspaceQueueStatus, bool, string) {
	localQueue := &unstructured.Unstructured{}
	localQueue.SetGroupVersionKind(localQueueGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: workspace.Spec.Queue, Namespace: targetNamespace}, localQueue); err != nil {
		if !apierrors.IsNotFound(err) {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue}, false, err.Error()
		}
		clusterQueue := &unstructured.Unstructured{}
		clusterQueue.SetGroupVersionKind(clusterQueueGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: workspace.Spec.Queue}, clusterQueue); err != nil {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: workspace.Spec.Queue}, false,
				fmt.Sprintf("backing ClusterQueue %q is not ready: %v", workspace.Spec.Queue, err)
		}
		localQueue = &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": localQueueGVK.GroupVersion().String(),
			"kind":       localQueueGVK.Kind,
			"metadata": map[string]any{
				"name":      workspace.Spec.Queue,
				"namespace": targetNamespace,
			},
			"spec": map[string]any{
				"clusterQueue": workspace.Spec.Queue,
			},
		}}
		localQueue.SetGroupVersionKind(localQueueGVK)
		localQueue.SetLabels(workspaceLabels(workspace.Name))
		if err := r.Create(ctx, localQueue); err != nil {
			if apierrors.IsAlreadyExists(err) {
				return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: workspace.Spec.Queue}, false,
					"workspace LocalQueue changed concurrently; retrying"
			}
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: workspace.Spec.Queue}, false,
				fmt.Sprintf("failed to reconcile workspace LocalQueue: %v", err)
		}
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: workspace.Spec.Queue}, true,
			"workspace LocalQueue is reconciled"
	}
	clusterQueueName, _, _ := unstructured.NestedString(localQueue.Object, "spec", "clusterQueue")
	labels := localQueue.GetLabels()
	if labels[labelManagedBy] == labelManagedByValue && labels[labelWorkspace] != workspace.Name {
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: clusterQueueName}, false,
			fmt.Sprintf("LocalQueue %q is owned by TauWorkspace %q", workspace.Spec.Queue, labels[labelWorkspace])
	}
	if labels[labelManagedBy] == labelManagedByValue && labels[labelWorkspace] == workspace.Name {
		desiredClusterQueue := workspace.Spec.Queue
		clusterQueue := &unstructured.Unstructured{}
		clusterQueue.SetGroupVersionKind(clusterQueueGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: desiredClusterQueue}, clusterQueue); err != nil {
			return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: desiredClusterQueue}, false,
				fmt.Sprintf("backing ClusterQueue %q is not ready: %v", desiredClusterQueue, err)
		}
		if clusterQueueName != desiredClusterQueue {
			if err := unstructured.SetNestedField(localQueue.Object, desiredClusterQueue, "spec", "clusterQueue"); err != nil {
				return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: desiredClusterQueue}, false,
					fmt.Sprintf("failed to restore workspace LocalQueue: %v", err)
			}
			if err := r.Update(ctx, localQueue); err != nil {
				return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: desiredClusterQueue}, false,
					fmt.Sprintf("failed to restore workspace LocalQueue: %v", err)
			}
			clusterQueueName = desiredClusterQueue
		}
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: clusterQueueName}, true,
			"workspace LocalQueue is reconciled"
	}
	if strings.TrimSpace(clusterQueueName) == "" {
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue}, false,
			fmt.Sprintf("LocalQueue %q does not reference a ClusterQueue", workspace.Spec.Queue)
	}
	clusterQueue := &unstructured.Unstructured{}
	clusterQueue.SetGroupVersionKind(clusterQueueGVK)
	if err := r.Get(ctx, client.ObjectKey{Name: clusterQueueName}, clusterQueue); err != nil {
		return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: clusterQueueName}, false,
			fmt.Sprintf("backing ClusterQueue %q is not ready: %v", clusterQueueName, err)
	}
	return tauv1alpha1.WorkspaceQueueStatus{LocalQueue: workspace.Spec.Queue, ClusterQueue: clusterQueueName}, true,
		"workspace queue and backing ClusterQueue are readable"
}

// reservedNamespaceReason names why a namespace may never be adopted as a
// workspace target, or "" when the namespace is fair game. Adoption is not
// read-only: reconcileNamespace stamps ownership labels onto whatever it
// resolves, and a later retarget hands the previous namespace to
// cleanupStaleNamespaceMetadata, which strips them again. Pointing a workspace
// at the platform or a kube-system namespace would therefore overwrite the
// Helm ownership metadata and drop the Kueue default-queue label of a
// namespace TauGrid does not own.
func reservedNamespaceReason(name, platformNamespace string) string {
	switch {
	case name == platformNamespace, name == tauv1alpha1.PlatformNamespace:
		return "the Tau platform namespace is owned by the platform installer"
	case strings.HasPrefix(name, "kube-"):
		return "kube-* namespaces are reserved by Kubernetes"
	case name == metav1.NamespaceDefault:
		return "the default namespace is shared cluster-wide"
	default:
		return ""
	}
}

func workspaceLabels(workspace string) map[string]string {
	return map[string]string{
		labelManagedBy: labelManagedByValue,
		labelWorkspace: workspace,
	}
}

func workspaceNamespaceLabels(workspace, localQueue string) map[string]string {
	labels := workspaceLabels(workspace)
	labels[labelWorkspaceLocalQueue] = localQueue
	labels[labelKueueDefaultLocalQueue] = localQueue
	labels[labelPSAEnforce] = "baseline"
	labels[labelPSAAudit] = "restricted"
	labels[labelPSAWarn] = "restricted"
	return labels
}

func workspaceNamespaceAnnotations(resultScope string) map[string]string {
	if strings.TrimSpace(resultScope) == "" {
		return nil
	}
	return map[string]string{annotationResultScope: resultScope}
}

func (r *TauWorkspaceReconciler) requireWorkspaceOwnershipOrAbsence(ctx context.Context, obj client.Object, workspaceName string) error {
	if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	labels := obj.GetLabels()
	if labels[labelManagedBy] == labelManagedByValue && labels[labelWorkspace] == workspaceName {
		return nil
	}
	return fmt.Errorf("%T %s already exists and is not owned by workspace %q", obj, client.ObjectKeyFromObject(obj), workspaceName)
}

func (r *TauWorkspaceReconciler) cleanupStaleNamespaceMetadata(ctx context.Context, workspaceName, namespaceName string) error {
	// The previous namespace comes off status and never passes through
	// reconcileNamespace, so it needs its own reserved-namespace check: a
	// workspace that once targeted a reserved namespace must not strip its
	// labels on the way out.
	if reservedNamespaceReason(namespaceName, r.platformNamespace()) != "" {
		return nil
	}
	var namespace corev1.Namespace
	if err := r.Get(ctx, client.ObjectKey{Name: namespaceName}, &namespace); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if namespace.Labels[labelWorkspace] != workspaceName {
		return nil
	}
	for _, key := range []string{
		labelManagedBy,
		labelWorkspace,
		labelWorkspaceLocalQueue,
		labelKueueDefaultLocalQueue,
	} {
		delete(namespace.Labels, key)
	}
	if namespace.Annotations != nil {
		delete(namespace.Annotations, annotationResultScope)
	}
	return r.Update(ctx, &namespace)
}

func (r *TauWorkspaceReconciler) cleanupWorkspaceAccess(ctx context.Context, workspaceName string) error {
	if err := r.cleanupStaleTargetRBAC(ctx, workspaceName, "", "", false); err != nil {
		return err
	}
	if err := r.cleanupStaleWorkspaceLocalQueues(ctx, workspaceName, "", ""); err != nil {
		return err
	}
	return r.cleanupPlatformReaderRBAC(ctx, workspaceName)
}

// ownerWorkspaceAbsent reports whether the TauWorkspace named on a namespace's
// ownership label no longer exists. Namespace ownership metadata is retained on
// deletion, so this check allows a later workspace to reclaim an orphaned target.
func (r *TauWorkspaceReconciler) ownerWorkspaceAbsent(ctx context.Context, owner string) (bool, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var existing tauv1alpha1.TauWorkspace
	err := reader.Get(ctx, client.ObjectKey{Name: owner, Namespace: r.platformNamespace()}, &existing)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return false, nil
}

func (r *TauWorkspaceReconciler) cleanupStaleTargetRBAC(
	ctx context.Context,
	workspaceName, keepNamespace, keepServiceAccount string,
	keepResearcherBinding bool,
) error {
	lists := []struct {
		list client.ObjectList
		keep func(client.Object) bool
	}{
		{list: &rbacv1.RoleList{}, keep: func(client.Object) bool { return false }},
		{
			list: &rbacv1.RoleBindingList{},
			keep: func(obj client.Object) bool {
				return keepResearcherBinding && obj.GetName() == defaultRoleName
			},
		},
		{
			list: &corev1.ServiceAccountList{},
			keep: func(obj client.Object) bool {
				return keepServiceAccount != "" && obj.GetName() == keepServiceAccount
			},
		},
	}
	for _, candidate := range lists {
		if err := r.List(ctx, candidate.list, client.MatchingLabels{
			labelManagedBy: labelManagedByValue,
			labelWorkspace: workspaceName,
		}); err != nil {
			return err
		}
		items, err := meta.ExtractList(candidate.list)
		if err != nil {
			return err
		}
		for _, item := range items {
			obj, ok := item.(client.Object)
			if !ok {
				continue
			}
			if obj.GetNamespace() == r.platformNamespace() {
				continue
			}
			if keepNamespace != "" && obj.GetNamespace() == keepNamespace && candidate.keep(obj) {
				continue
			}
			if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}
	return nil
}

func (r *TauWorkspaceReconciler) cleanupStaleWorkspaceLocalQueues(ctx context.Context, workspaceName, keepNamespace, keepName string) error {
	localQueues := &unstructured.UnstructuredList{}
	localQueues.SetGroupVersionKind(schema.GroupVersionKind{
		Group: localQueueGVK.Group, Version: localQueueGVK.Version, Kind: localQueueGVK.Kind + "List",
	})
	if err := r.List(ctx, localQueues, client.MatchingLabels{
		labelManagedBy: labelManagedByValue,
		labelWorkspace: workspaceName,
	}); err != nil {
		return err
	}
	for i := range localQueues.Items {
		localQueue := &localQueues.Items[i]
		if localQueue.GetNamespace() == keepNamespace && localQueue.GetName() == keepName {
			continue
		}
		if err := r.Delete(ctx, localQueue); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *TauWorkspaceReconciler) cleanupPlatformReaderRBAC(ctx context.Context, workspaceName string) error {
	name := "tau-workspace-reader-" + workspaceName
	for _, obj := range []client.Object{
		&rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.platformNamespace()}},
		&rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.platformNamespace()}},
	} {
		if err := r.deleteOwnedObject(ctx, obj, workspaceName); err != nil {
			return err
		}
	}
	return nil
}

func (r *TauWorkspaceReconciler) deleteOwnedObject(ctx context.Context, obj client.Object, workspaceName string) error {
	key := client.ObjectKeyFromObject(obj)
	if err := r.Get(ctx, key, obj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	labels := obj.GetLabels()
	if labels[labelManagedBy] != labelManagedByValue || labels[labelWorkspace] != workspaceName {
		return fmt.Errorf("refusing to delete %T %s: object is not owned by workspace %q", obj, key, workspaceName)
	}
	if err := r.Delete(ctx, obj); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func reasonFor(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}
