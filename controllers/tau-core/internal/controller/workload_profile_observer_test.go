// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	profile "github.com/Azure/taugrid/core/resourceprofile"
	schedulingv1 "k8s.io/api/scheduling/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestWorkloadProfilesResolveInObserveAndReconcileModesWithoutKueueMutations(t *testing.T) {
	for _, mode := range []string{
		tauv1alpha1.ClusterManagementModeObserve,
		tauv1alpha1.ClusterManagementModeReconcile,
	} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			cluster := testProfileCluster(mode, []profile.WorkloadProfile{testWorkloadProfile("research", []string{"team-b", "team-a"})})
			objects := append([]client.Object{cluster}, validProfileDependencies("research", "team-a", "team-b")...)
			baseClient := fake.NewClientBuilder().
				WithScheme(testScheme(t)).
				WithObjects(objects...).
				WithStatusSubresource(&tauv1alpha1.TauCluster{}).
				Build()
			recordingClient := &resourceMutationRecordingClient{Client: baseClient}
			reconciler := &TauClusterReconciler{Client: recordingClient}

			if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if len(recordingClient.mutations) != 0 {
				t.Fatalf("profile observation mutated resources: %v", recordingClient.mutations)
			}
			if err := baseClient.Get(ctx, client.ObjectKey{Name: cluster.Name}, cluster); err != nil {
				t.Fatalf("Get TauCluster: %v", err)
			}
			got := cluster.Status.WorkloadProfiles
			if got.ObservedGeneration != cluster.Generation || got.Observed != 1 || got.Ready != 1 || got.Drifted != 0 {
				t.Fatalf("workload profile counts = %#v", got)
			}
			if got.ProfileSetHash == "" {
				t.Fatal("profileSetHash is empty")
			}
			if len(got.Profiles) != 1 {
				t.Fatalf("resolved profiles = %#v", got.Profiles)
			}
			resolved := got.Profiles[0]
			if got, want := resolved.LocalQueues, []profile.ResolvedLocalQueue{
				{Namespace: "team-a", Name: "research", ClusterQueue: "research-cq"},
				{Namespace: "team-b", Name: "research", ClusterQueue: "research-cq"},
			}; !reflect.DeepEqual(got, want) {
				t.Fatalf("LocalQueues = %#v, want %#v", got, want)
			}
			if !reflect.DeepEqual(resolved.ClusterQueues, []string{"research-cq"}) ||
				!reflect.DeepEqual(resolved.ResourceFlavors, []string{"a100", "h200"}) ||
				!reflect.DeepEqual(resolved.Topologies, []string{"gpu-topology"}) ||
				!reflect.DeepEqual(resolved.WorkloadPriorityClasses, []string{"research-priority"}) ||
				!reflect.DeepEqual(resolved.PodPriorityClasses, []string{"research-pod-priority"}) {
				t.Fatalf("resolved identities = %#v", resolved)
			}
			assertCondition(t, resolved.Conditions, profile.ConditionReady, metav1.ConditionTrue)
			for _, conditionType := range []string{
				profile.ConditionLocalQueuesResolved,
				profile.ConditionClusterQueuesReady,
				profile.ConditionResourceFlavorsReady,
				profile.ConditionTopologiesReady,
				profile.ConditionPriorityClassesReady,
				profile.ConditionExecutionReady,
			} {
				condition := findCondition(resolved.Conditions, conditionType)
				if condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != cluster.Generation {
					t.Fatalf("condition %s = %#v", conditionType, condition)
				}

			}
			assertCondition(t, cluster.Status.Conditions, tauv1alpha1.ConditionWorkloadProfilesReady, metav1.ConditionTrue)
		})
	}
}

func TestClusterQueueAdmissionCheckNamesSupportsKueueShapes(t *testing.T) {
	clusterQueue := testObservedClusterQueue("research-cq", nil, true)
	clusterQueue.Object["spec"].(map[string]any)["admissionChecksStrategy"] = map[string]any{
		"admissionChecks": []any{
			map[string]any{"name": "strategy-b"},
			map[string]any{"name": "strategy-a"},
		},
	}
	got, err := clusterQueueAdmissionCheckNames(clusterQueue)
	if err != nil {
		t.Fatalf("clusterQueueAdmissionCheckNames() error = %v", err)
	}
	if want := []string{"strategy-a", "strategy-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("admission check names = %v, want %v", got, want)
	}

	delete(clusterQueue.Object["spec"].(map[string]any), "admissionChecksStrategy")
	clusterQueue.Object["spec"].(map[string]any)["admissionChecks"] = []any{"legacy-b", "legacy-a"}
	got, err = clusterQueueAdmissionCheckNames(clusterQueue)
	if err != nil {
		t.Fatalf("legacy clusterQueueAdmissionCheckNames() error = %v", err)
	}
	if want := []string{"legacy-a", "legacy-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy admission check names = %v, want %v", got, want)
	}

	clusterQueue.Object["spec"].(map[string]any)["admissionChecksStrategy"] = map[string]any{
		"admissionChecks": []any{map[string]any{"name": "strategy"}},
	}
	if _, err := clusterQueueAdmissionCheckNames(clusterQueue); err == nil {
		t.Fatal("simultaneous legacy and strategy admission checks were accepted")
	}

	delete(clusterQueue.Object["spec"].(map[string]any), "admissionChecks")
	clusterQueue.Object["spec"].(map[string]any)["admissionChecksStrategy"] = map[string]any{
		"admissionChecks": []any{map[string]any{"name": int64(7)}},
	}
	if _, err := clusterQueueAdmissionCheckNames(clusterQueue); err == nil {
		t.Fatal("malformed v1beta2 admission-check strategy rule was accepted")
	}
}

func TestSingleClusterProfileRejectsOnlyExactMultiKueueController(t *testing.T) {
	tests := []struct {
		name       string
		controller string
		wantReady  bool
	}{
		{name: "exact", controller: multiKueueAdmissionCheckController, wantReady: false},
		{name: "name is not identity", controller: "example.com/generic", wantReady: true},
		{name: "controller prefix is not identity", controller: multiKueueAdmissionCheckController + "/other", wantReady: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cluster := testProfileCluster(tauv1alpha1.ClusterManagementModeObserve, []profile.WorkloadProfile{
				testWorkloadProfile("research", []string{"team-a"}),
			})
			dependencies := profileDependenciesWithAdmissionChecks(
				"research",
				"team-a",
				map[string]string{"multikueue": test.controller},
			)
			reconciler := &TauClusterReconciler{
				Client: fake.NewClientBuilder().
					WithScheme(testScheme(t)).
					WithObjects(dependencies...).
					Build(),
			}

			state, err := reconciler.observeWorkloadProfiles(context.Background(), cluster)
			if err != nil {
				t.Fatalf("observeWorkloadProfiles() error = %v", err)
			}
			execution := findCondition(state.status.Profiles[0].Conditions, profile.ConditionExecutionReady)
			if execution == nil || (execution.Status == metav1.ConditionTrue) != test.wantReady {
				t.Fatalf("ExecutionReady = %#v, want ready %v", execution, test.wantReady)
			}
			if !test.wantReady && execution.Reason != "UnexpectedMultiKueueAdmissionCheck" {
				t.Fatalf("ExecutionReady reason = %q", execution.Reason)
			}
		})
	}
}

func TestAdmissionCheckLookupFailuresFailClosed(t *testing.T) {
	tests := []struct {
		name       string
		objects    func() []client.Object
		wrapClient func(client.Client) client.Client
		wantErr    bool
		wantText   string
	}{
		{
			name: "missing",
			objects: func() []client.Object {
				return omitProfileDependency(
					profileDependenciesWithAdmissionChecks("research", "team-a", map[string]string{"gate": ""}),
					"admissioncheck/gate",
				)
			},
			wantText: "does not exist",
		},
		{
			name: "unreadable",
			objects: func() []client.Object {
				return profileDependenciesWithAdmissionChecks("research", "team-a", map[string]string{"gate": "example.com/gate"})
			},
			wrapClient: func(base client.Client) client.Client {
				return &admissionCheckReadErrorClient{Client: base}
			},
			wantErr:  true,
			wantText: "cannot read AdmissionCheck gate",
		},
		{
			name: "malformed",
			objects: func() []client.Object {
				objects := profileDependenciesWithAdmissionChecks("research", "team-a", map[string]string{"gate": "example.com/gate"})
				for _, object := range objects {
					if objectKey(object) == "admissioncheck/gate" {
						object.(*unstructured.Unstructured).Object["spec"] = map[string]any{"controllerName": int64(7)}
					}
				}
				return objects
			},
			wantText: "is malformed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(test.objects()...).Build()
			var observationClient client.Client = base
			if test.wrapClient != nil {
				observationClient = test.wrapClient(base)
			}
			cluster := testProfileCluster(tauv1alpha1.ClusterManagementModeObserve, []profile.WorkloadProfile{
				testWorkloadProfile("research", []string{"team-a"}),
			})
			state, err := (&TauClusterReconciler{
				Client: observationClient,
			}).observeWorkloadProfiles(context.Background(), cluster)
			if (err != nil) != test.wantErr {
				t.Fatalf("observeWorkloadProfiles() error = %v, wantErr %v", err, test.wantErr)
			}
			execution := findCondition(state.status.Profiles[0].Conditions, profile.ConditionExecutionReady)
			if execution == nil || execution.Status != metav1.ConditionFalse ||
				execution.Reason != "AdmissionChecksNotReady" ||
				!strings.Contains(execution.Message, test.wantText) {
				t.Fatalf("ExecutionReady = %#v, want failure containing %q", execution, test.wantText)
			}
		})
	}
}

func TestMultiKueueExecutionReadiness(t *testing.T) {
	tests := []struct {
		name          string
		mutateCluster func(*tauv1alpha1.TauCluster)
		mutateProfile func(*profile.WorkloadProfile)
		checks        map[string]string
		wantReady     bool
		wantReason    string
		wantMessage   string
	}{
		{
			name: "without wiring", checks: nil,
			wantReason: "MultiKueueWiringNotReady", wantMessage: "has no admission checks",
		},
		{
			name: "mixed wiring",
			checks: map[string]string{
				"multikueue": multiKueueAdmissionCheckController,
				"provision":  "kueue.x-k8s.io/provisioning-request",
			},
			wantReason: "MultiKueueWiringNotReady", wantMessage: "does not use only MultiKueue",
		},
		{
			name:   "missing capability",
			checks: map[string]string{"multikueue": multiKueueAdmissionCheckController},
			mutateCluster: func(cluster *tauv1alpha1.TauCluster) {
				cluster.Status.Conditions = nil
			},
			wantReason: "PrerequisitesNotReady", wantMessage: "no MultiKueueReady",
		},
		{
			name:   "stale capability",
			checks: map[string]string{"multikueue": multiKueueAdmissionCheckController},
			mutateCluster: func(cluster *tauv1alpha1.TauCluster) {
				cluster.Status.Conditions[0].ObservedGeneration--
			},
			wantReason: "PrerequisitesNotReady", wantMessage: "want current generation",
		},
		{
			name:   "false capability",
			checks: map[string]string{"multikueue": multiKueueAdmissionCheckController},
			mutateCluster: func(cluster *tauv1alpha1.TauCluster) {
				cluster.Status.Conditions[0].Status = metav1.ConditionFalse
				cluster.Status.Conditions[0].Reason = "PrerequisitesNotReady"
				cluster.Status.Conditions[0].Message = "manager prerequisites are not ready"
			},
			wantReason: "PrerequisitesNotReady", wantMessage: "manager prerequisites",
		},
		{
			name:   "default queue allowed",
			checks: map[string]string{"multikueue": multiKueueAdmissionCheckController},
			mutateCluster: func(cluster *tauv1alpha1.TauCluster) {
				cluster.Spec.WorkspaceDefaults.DefaultQueue = "research"
			},
			wantReady: true, wantReason: "Ready",
		},
		{
			name:   "global team applicability allowed",
			checks: map[string]string{"multikueue": multiKueueAdmissionCheckController},
			mutateProfile: func(workloadProfile *profile.WorkloadProfile) {
				workloadProfile.Applicability.Teams = nil
			},
			wantReady: true, wantReason: "Ready",
		},
		{
			name:      "valid",
			checks:    map[string]string{"multikueue": multiKueueAdmissionCheckController},
			wantReady: true, wantReason: "Ready",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			workloadProfile := testMultiKueueProfile("research", "team-a")
			if test.mutateProfile != nil {
				test.mutateProfile(&workloadProfile)
			}
			cluster := testProfileCluster(tauv1alpha1.ClusterManagementModeObserve, []profile.WorkloadProfile{workloadProfile})
			cluster.Status.Conditions = []metav1.Condition{{
				Type:               tauv1alpha1.ConditionMultiKueueReady,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: cluster.Generation,
				Reason:             "Ready",
				Message:            "MultiKueue prerequisites are ready",
			}}
			if test.mutateCluster != nil {
				test.mutateCluster(cluster)
			}
			var dependencies []client.Object
			if test.checks == nil {
				dependencies = validProfileDependencies("research", "team-a")
			} else {
				dependencies = profileDependenciesWithAdmissionChecks("research", "team-a", test.checks)
			}
			reconciler := &TauClusterReconciler{
				Client: fake.NewClientBuilder().
					WithScheme(testScheme(t)).
					WithObjects(dependencies...).
					Build(),
			}

			state, err := reconciler.observeWorkloadProfiles(context.Background(), cluster)
			if err != nil {
				t.Fatalf("observeWorkloadProfiles() error = %v", err)
			}
			resolved := state.status.Profiles[0]
			execution := findCondition(resolved.Conditions, profile.ConditionExecutionReady)
			if execution == nil || (execution.Status == metav1.ConditionTrue) != test.wantReady ||
				execution.Reason != test.wantReason || !strings.Contains(execution.Message, test.wantMessage) {
				t.Fatalf("execution condition = %#v", execution)
			}
		})
	}
}

func TestMultiKueueProfileRequiresItsReferencedWorkerToBeReady(t *testing.T) {
	cluster := testProfileCluster(tauv1alpha1.ClusterManagementModeObserve, []profile.WorkloadProfile{
		testMultiKueueProfile("research", "team-a"),
	})
	cluster.Status.Conditions = []metav1.Condition{{
		Type:               tauv1alpha1.ConditionMultiKueueReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cluster.Generation,
		Reason:             "Ready",
		Message:            "an unrelated MultiKueue AdmissionCheck is ready",
	}}
	dependencies := profileDependenciesWithAdmissionChecks(
		"research",
		"team-a",
		map[string]string{"profile-check": multiKueueAdmissionCheckController},
	)
	for _, object := range dependencies {
		if objectKey(object) != "multikueuecluster/profile-check-worker" {
			continue
		}
		object.(*unstructured.Unstructured).Object["status"] = map[string]any{"conditions": []any{
			map[string]any{"type": "Active", "status": "False"},
		}}
	}
	reconciler := &TauClusterReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(testScheme(t)).
			WithObjects(dependencies...).
			Build(),
	}

	state, err := reconciler.observeWorkloadProfiles(context.Background(), cluster)
	if err != nil {
		t.Fatalf("observeWorkloadProfiles() error = %v", err)
	}
	execution := findCondition(
		state.status.Profiles[0].Conditions,
		profile.ConditionExecutionReady,
	)
	if execution == nil || execution.Status != metav1.ConditionFalse ||
		execution.Reason != "AdmissionChecksNotReady" ||
		!strings.Contains(execution.Message, `MultiKueueConfig "profile-check-config" has no Active worker clusters`) {
		t.Fatalf("ExecutionReady = %#v", execution)
	}
}

func TestValidMultiKueueProfileObservationDoesNotMutateResources(t *testing.T) {
	ctx := context.Background()
	workloadProfile := testMultiKueueProfile("research", "team-a")
	cluster := testProfileCluster(tauv1alpha1.ClusterManagementModeReconcile, []profile.WorkloadProfile{workloadProfile})
	cluster.Status.Conditions = []metav1.Condition{{
		Type:               tauv1alpha1.ConditionMultiKueueReady,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cluster.Generation,
		Reason:             "Ready",
	}}
	objects := append([]client.Object{cluster}, profileDependenciesWithAdmissionChecks(
		"research",
		"team-a",
		map[string]string{"multikueue": multiKueueAdmissionCheckController},
	)...)
	baseClient := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	recordingClient := &resourceMutationRecordingClient{Client: baseClient}

	if _, err := (&TauClusterReconciler{
		Client: recordingClient,
		MultiKueuePrerequisites: &countingMultiKueuePrerequisiteReader{status: MultiKueuePrerequisiteStatus{
			Ready:   true,
			Message: "manager prerequisites are ready",
		}},
	}).Reconcile(
		ctx,
		ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}},
	); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if len(recordingClient.mutations) != 0 {
		t.Fatalf("MultiKueue profile observation mutated resources: %v", recordingClient.mutations)
	}
}

func TestWorkloadProfileInvalidIntentIsReported(t *testing.T) {
	declared := testWorkloadProfile("broken", []string{"team-a"})
	declared.WorkerCount = 0
	cluster := testProfileCluster(tauv1alpha1.ClusterManagementModeObserve, []profile.WorkloadProfile{declared})
	state, err := (&TauClusterReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}).
		observeWorkloadProfiles(context.Background(), cluster)
	if err != nil {
		t.Fatalf("observeWorkloadProfiles() error = %v", err)
	}
	if state.status.Observed != 1 || state.status.Ready != 0 || state.status.Drifted != 1 {
		t.Fatalf("status = %#v", state.status)
	}
	if state.status.ProfileSetHash != "" {
		t.Fatalf("invalid profile hash = %q, want empty", state.status.ProfileSetHash)
	}
	ready := findCondition(state.status.Profiles[0].Conditions, profile.ConditionReady)
	if ready == nil || ready.Status != metav1.ConditionFalse || ready.Reason != "InvalidWorkloadProfile" ||
		!strings.Contains(ready.Message, "workerCount must be positive") {
		t.Fatalf("Ready = %#v", ready)
	}
}

func TestWorkloadProfileMissingDependenciesAreActionable(t *testing.T) {
	tests := []struct {
		name          string
		omit          string
		conditionType string
		message       string
	}{
		{name: "LocalQueue", omit: "localqueue/team-a/research", conditionType: profile.ConditionLocalQueuesResolved, message: "LocalQueue team-a/research does not exist"},
		{name: "ClusterQueue", omit: "clusterqueue/research-cq", conditionType: profile.ConditionClusterQueuesReady, message: "ClusterQueue research-cq does not exist"},
		{name: "ResourceFlavor", omit: "resourceflavor/h200", conditionType: profile.ConditionResourceFlavorsReady, message: "ResourceFlavor h200 does not exist"},
		{name: "Topology", omit: "topology/gpu-topology", conditionType: profile.ConditionTopologiesReady, message: "Topology gpu-topology does not exist"},
		{name: "WorkloadPriorityClass", omit: "workloadpriorityclass/research-priority", conditionType: profile.ConditionPriorityClassesReady, message: "WorkloadPriorityClass research-priority does not exist"},
		{name: "PriorityClass", omit: "priorityclass/research-pod-priority", conditionType: profile.ConditionPriorityClassesReady, message: "PriorityClass research-pod-priority does not exist"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cluster := testProfileCluster(tauv1alpha1.ClusterManagementModeObserve, []profile.WorkloadProfile{testWorkloadProfile("research", []string{"team-a"})})
			dependencies := omitProfileDependency(validProfileDependencies("research", "team-a"), test.omit)
			reconciler := &TauClusterReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(dependencies...).Build()}

			state, err := reconciler.observeWorkloadProfiles(context.Background(), cluster)
			if err != nil {
				t.Fatalf("observeWorkloadProfiles() error = %v", err)
			}
			resolved := state.status.Profiles[0]
			dependencyCondition := findCondition(resolved.Conditions, test.conditionType)
			if dependencyCondition == nil || dependencyCondition.Status != metav1.ConditionFalse ||
				!strings.Contains(dependencyCondition.Message, test.message) {
				t.Fatalf("%s = %#v, want message containing %q", test.conditionType, dependencyCondition, test.message)
			}
			assertCondition(t, resolved.Conditions, profile.ConditionReady, metav1.ConditionFalse)
			if state.condition.Status != metav1.ConditionFalse {
				t.Fatalf("WorkloadProfilesReady = %#v", state.condition)
			}
		})
	}
}

func TestWorkloadProfileRejectsInactiveClusterQueue(t *testing.T) {
	cluster := testProfileCluster(tauv1alpha1.ClusterManagementModeObserve, []profile.WorkloadProfile{testWorkloadProfile("research", []string{"team-a"})})
	dependencies := validProfileDependencies("research", "team-a")
	for _, object := range dependencies {
		if objectKey(object) == "clusterqueue/research-cq" {
			clusterQueue := object.(*unstructured.Unstructured)
			clusterQueue.Object["status"] = map[string]any{"conditions": []any{map[string]any{
				"type": "Active", "status": "False", "reason": "FlavorNotFound", "message": "a flavor is unavailable",
			}}}
		}
	}
	reconciler := &TauClusterReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(dependencies...).Build()}

	state, err := reconciler.observeWorkloadProfiles(context.Background(), cluster)
	if err != nil {
		t.Fatalf("observeWorkloadProfiles() error = %v", err)
	}
	condition := findCondition(state.status.Profiles[0].Conditions, profile.ConditionClusterQueuesReady)
	if condition == nil || condition.Status != metav1.ConditionFalse ||
		!strings.Contains(condition.Message, "inactive: FlavorNotFound: a flavor is unavailable") {
		t.Fatalf("ClusterQueuesReady = %#v", condition)
	}
}

func TestWorkloadProfileRejectsClusterQueueWithoutActiveCondition(t *testing.T) {
	tests := []struct {
		name        string
		status      map[string]any
		wantMessage string
	}{
		{
			name:        "status absent",
			wantMessage: "status.conditions is not reported",
		},
		{
			name: "Active absent",
			status: map[string]any{"conditions": []any{map[string]any{
				"type": "PodsReady", "status": "True",
			}}},
			wantMessage: "Active condition is not reported",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cluster := testProfileCluster(tauv1alpha1.ClusterManagementModeObserve, []profile.WorkloadProfile{
				testWorkloadProfile("research", []string{"team-a"}),
			})
			dependencies := validProfileDependencies("research", "team-a")
			for _, object := range dependencies {
				if objectKey(object) != "clusterqueue/research-cq" {
					continue
				}
				clusterQueue := object.(*unstructured.Unstructured)
				if test.status == nil {
					delete(clusterQueue.Object, "status")
				} else {
					clusterQueue.Object["status"] = test.status
				}
			}
			reconciler := &TauClusterReconciler{
				Client: fake.NewClientBuilder().
					WithScheme(testScheme(t)).
					WithObjects(dependencies...).
					Build(),
			}

			state, err := reconciler.observeWorkloadProfiles(context.Background(), cluster)
			if err != nil {
				t.Fatalf("observeWorkloadProfiles() error = %v", err)
			}
			condition := findCondition(
				state.status.Profiles[0].Conditions,
				profile.ConditionClusterQueuesReady,
			)
			if condition == nil || condition.Status != metav1.ConditionFalse ||
				condition.Reason != "ClusterQueuesNotReady" ||
				!strings.Contains(condition.Message, test.wantMessage) {
				t.Fatalf("ClusterQueuesReady = %#v, want message containing %q", condition, test.wantMessage)
			}
		})
	}
}

func TestWorkloadProfileGlobalApplicabilityUsesOnlyDeclaredSharedQueues(t *testing.T) {
	global := testWorkloadProfile("shared", nil)
	global.Priorities.DisableDefaultPriorities = true
	global.Priorities.WorkloadPriorityClassName = ""
	global.Priorities.PodPriorityClassName = ""
	cluster := testProfileCluster(tauv1alpha1.ClusterManagementModeObserve, []profile.WorkloadProfile{global})
	cluster.Spec.Queues.SharedLocalQueues = []tauv1alpha1.TauNamespacedObjectReference{
		{Namespace: "zeta", Name: "other"},
		{Namespace: "beta", Name: "shared"},
		{Namespace: "alpha", Name: "shared"},
	}
	dependencies := []client.Object{
		testLocalQueue("alpha", "shared", "shared-cq"),
		testLocalQueue("beta", "shared", "shared-cq"),
		testObservedClusterQueue("shared-cq", nil, true),
	}
	reconciler := &TauClusterReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(dependencies...).Build()}

	state, err := reconciler.observeWorkloadProfiles(context.Background(), cluster)
	if err != nil {
		t.Fatalf("observeWorkloadProfiles() error = %v", err)
	}
	got := state.status.Profiles[0].LocalQueues
	want := []profile.ResolvedLocalQueue{
		{Namespace: "alpha", Name: "shared", ClusterQueue: "shared-cq"},
		{Namespace: "beta", Name: "shared", ClusterQueue: "shared-cq"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("LocalQueues = %#v, want %#v", got, want)
	}
	assertCondition(t, state.status.Profiles[0].Conditions, profile.ConditionReady, metav1.ConditionTrue)

	cluster.Spec.Queues.SharedLocalQueues = nil
	state, err = reconciler.observeWorkloadProfiles(context.Background(), cluster)
	if err != nil {
		t.Fatalf("observe without shared queues error = %v", err)
	}
	localCondition := findCondition(state.status.Profiles[0].Conditions, profile.ConditionLocalQueuesResolved)
	if localCondition == nil || localCondition.Status != metav1.ConditionFalse ||
		!strings.Contains(localCondition.Message, "sharedLocalQueues declares no") {
		t.Fatalf("LocalQueuesResolved = %#v", localCondition)
	}
}

func TestWorkloadProfileHashAndOrderAreDeterministic(t *testing.T) {
	leftProfiles := []profile.WorkloadProfile{
		testWorkloadProfile("zeta", []string{"team-b"}),
		testWorkloadProfile("alpha", []string{"team-a"}),
	}
	rightProfiles := []profile.WorkloadProfile{leftProfiles[1], leftProfiles[0]}
	dependencies := append(validProfileDependencies("zeta", "team-b"), validProfileDependencies("alpha", "team-a")...)
	dependencies = uniqueProfileDependencies(dependencies)
	reconciler := &TauClusterReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(dependencies...).Build()}

	left, err := reconciler.observeWorkloadProfiles(context.Background(), testProfileCluster(tauv1alpha1.ClusterManagementModeObserve, leftProfiles))
	if err != nil {
		t.Fatalf("left observation error = %v", err)
	}
	right, err := reconciler.observeWorkloadProfiles(context.Background(), testProfileCluster(tauv1alpha1.ClusterManagementModeObserve, rightProfiles))
	if err != nil {
		t.Fatalf("right observation error = %v", err)
	}
	if left.status.ProfileSetHash == "" || left.status.ProfileSetHash != right.status.ProfileSetHash {
		t.Fatalf("hashes differ: left=%q right=%q", left.status.ProfileSetHash, right.status.ProfileSetHash)
	}
	for _, status := range []profile.ProfileSetStatus{left.status, right.status} {
		if got := []string{status.Profiles[0].Name, status.Profiles[1].Name}; !reflect.DeepEqual(got, []string{"alpha", "zeta"}) {
			t.Fatalf("profile order = %v", got)
		}
	}
}

func TestWorkloadProfileStatusNoOpAndConditionTransition(t *testing.T) {
	ctx := context.Background()
	cluster := testProfileCluster(tauv1alpha1.ClusterManagementModeObserve, []profile.WorkloadProfile{testWorkloadProfile("research", []string{"team-a"})})
	objects := append([]client.Object{cluster}, validProfileDependencies("research", "team-a")...)
	baseClient := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(objects...).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	reconciler := &TauClusterReconciler{Client: baseClient}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}

	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if err := baseClient.Get(ctx, client.ObjectKey{Name: cluster.Name}, cluster); err != nil {
		t.Fatalf("Get TauCluster: %v", err)
	}
	before := cluster.DeepCopy().Status
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("no-op Reconcile() error = %v", err)
	}
	if err := baseClient.Get(ctx, client.ObjectKey{Name: cluster.Name}, cluster); err != nil {
		t.Fatalf("Get TauCluster after no-op: %v", err)
	}
	if !reflect.DeepEqual(before, cluster.Status) {
		t.Fatalf("status changed on no-op:\nbefore=%#v\nafter=%#v", before, cluster.Status)
	}

	clusterQueue := newQueueObject(clusterQueueGVK)
	if err := baseClient.Get(ctx, client.ObjectKey{Name: "research-cq"}, clusterQueue); err != nil {
		t.Fatalf("Get ClusterQueue: %v", err)
	}
	clusterQueue.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Active", "status": "False"}}}
	if err := baseClient.Update(ctx, clusterQueue); err != nil {
		t.Fatalf("Update ClusterQueue status: %v", err)
	}
	oldTransition := findCondition(before.WorkloadProfiles.Profiles[0].Conditions, profile.ConditionClusterQueuesReady).LastTransitionTime
	time.Sleep(time.Second)
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatalf("transition Reconcile() error = %v", err)
	}
	if err := baseClient.Get(ctx, client.ObjectKey{Name: cluster.Name}, cluster); err != nil {
		t.Fatalf("Get transitioned TauCluster: %v", err)
	}
	transitioned := findCondition(cluster.Status.WorkloadProfiles.Profiles[0].Conditions, profile.ConditionClusterQueuesReady)
	if transitioned == nil || transitioned.Status != metav1.ConditionFalse || transitioned.LastTransitionTime.Equal(&oldTransition) {
		t.Fatalf("transitioned condition = %#v, old transition = %v", transitioned, oldTransition)
	}
}

func TestEmptyWorkloadProfileSetIsReadyWithDeterministicHash(t *testing.T) {
	reconciler := &TauClusterReconciler{Client: fake.NewClientBuilder().WithScheme(testScheme(t)).Build()}
	state, err := reconciler.observeWorkloadProfiles(context.Background(), testProfileCluster(tauv1alpha1.ClusterManagementModeObserve, nil))
	if err != nil {
		t.Fatalf("observeWorkloadProfiles() error = %v", err)
	}
	if state.status.ProfileSetHash == "" || state.status.Observed != 0 || state.status.Ready != 0 || state.status.Drifted != 0 {
		t.Fatalf("empty status = %#v", state.status)
	}
	if state.condition.Status != metav1.ConditionTrue || state.condition.Reason != "NoWorkloadProfiles" {
		t.Fatalf("empty condition = %#v", state.condition)
	}
}

func TestFailedWorkloadProfileDoesNotChangeNodeBasedGlobalPhase(t *testing.T) {
	ctx := context.Background()
	cluster := testProfileCluster(tauv1alpha1.ClusterManagementModeReconcile, []profile.WorkloadProfile{
		testWorkloadProfile("missing", []string{"team-a"}),
	})
	baseClient := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(cluster).
		WithStatusSubresource(&tauv1alpha1.TauCluster{}).
		Build()
	reconciler := &TauClusterReconciler{Client: baseClient}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: cluster.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if err := baseClient.Get(ctx, client.ObjectKey{Name: cluster.Name}, cluster); err != nil {
		t.Fatalf("Get TauCluster: %v", err)
	}
	if cluster.Status.Phase != tauv1alpha1.ClusterPhaseReady {
		t.Fatalf("phase = %q, want node-based Ready", cluster.Status.Phase)
	}
	assertCondition(t, cluster.Status.Conditions, tauv1alpha1.ConditionReady, metav1.ConditionTrue)
	assertCondition(t, cluster.Status.Conditions, tauv1alpha1.ConditionWorkloadProfilesReady, metav1.ConditionFalse)
	assertCondition(t, cluster.Status.WorkloadProfiles.Profiles[0].Conditions, profile.ConditionReady, metav1.ConditionFalse)
}

func TestProfileDependencyEventsEnqueueOnlyTauClusterSingleton(t *testing.T) {
	requests := enqueueTauClusterForProfileDependency(context.Background(), testLocalQueue("team-a", "research", "research-cq"))
	want := []reconcile.Request{{NamespacedName: types.NamespacedName{Name: tauv1alpha1.TauClusterSingletonName}}}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests = %#v, want %#v", requests, want)
	}
}

func testProfileCluster(mode string, profiles []profile.WorkloadProfile) *tauv1alpha1.TauCluster {
	return &tauv1alpha1.TauCluster{
		ObjectMeta: metav1.ObjectMeta{Name: tauv1alpha1.TauClusterSingletonName, Generation: 7},
		Spec: tauv1alpha1.TauClusterSpec{
			ManagementMode:   mode,
			WorkloadProfiles: profiles,
		},
	}
}

func testWorkloadProfile(name string, namespaces []string) profile.WorkloadProfile {
	return profile.WorkloadProfile{
		Name: name,
		Applicability: profile.ProfileApplicability{
			Namespaces: namespaces,
		},
		GPUsPerWorker:     1,
		WorkerCount:       1,
		Mode:              profile.ModeFixed,
		Placement:         profile.PlacementIndependent,
		DefaultLocalQueue: name,
		Priorities: profile.ProfilePriorities{
			WorkloadPriorityClassName: name + "-priority",
			PodPriorityClassName:      name + "-pod-priority",
		},
	}
}

func testMultiKueueProfile(name string, namespaces ...string) profile.WorkloadProfile {
	workloadProfile := testWorkloadProfile(name, namespaces)
	workloadProfile.ExecutionTarget = profile.ExecutionTargetMultiKueue
	workloadProfile.Applicability.Teams = []string{"research"}
	return workloadProfile
}

func profileDependenciesWithAdmissionChecks(
	queueName string,
	namespace string,
	checks map[string]string,
) []client.Object {
	objects := validProfileDependencies(queueName, namespace)
	var clusterQueue *unstructured.Unstructured
	for _, object := range objects {
		if objectKey(object) == "clusterqueue/"+queueName+"-cq" {
			clusterQueue = object.(*unstructured.Unstructured)
			break
		}
	}
	entries := make([]any, 0, len(checks))
	for name, controllerName := range checks {
		entries = append(entries, map[string]any{"name": name})
		admissionCheck := testAdmissionCheck(name, controllerName)
		objects = append(objects, admissionCheck)
		if controllerName == multiKueueAdmissionCheckController {
			configName := name + "-config"
			workerName := name + "-worker"
			admissionCheck.Object["spec"] = map[string]any{
				"controllerName": controllerName,
				"parameters": map[string]any{
					"apiGroup": "kueue.x-k8s.io",
					"kind":     "MultiKueueConfig",
					"name":     configName,
				},
			}
			admissionCheck.Object["status"] = map[string]any{"conditions": []any{
				map[string]any{"type": "Active", "status": "True"},
			}}
			config := testKueueObject(multiKueueConfigGVK, configName)
			config.Object["spec"] = map[string]any{"clusters": []any{workerName}}
			worker := testKueueObject(multiKueueClusterGVK, workerName)
			worker.Object["status"] = map[string]any{"conditions": []any{
				map[string]any{"type": "Active", "status": "True"},
			}}
			objects = append(objects, config, worker)
		}
	}
	clusterQueue.Object["spec"].(map[string]any)["admissionChecksStrategy"] = map[string]any{
		"admissionChecks": entries,
	}
	return objects
}

func testAdmissionCheck(name, controllerName string) *unstructured.Unstructured {
	object := testKueueObject(admissionCheckGVK, name)
	object.Object["spec"] = map[string]any{"controllerName": controllerName}
	return object
}

type admissionCheckReadErrorClient struct {
	client.Client
	reads int
}

func (c *admissionCheckReadErrorClient) Get(
	ctx context.Context,
	key client.ObjectKey,
	object client.Object,
	options ...client.GetOption,
) error {
	if object.GetObjectKind().GroupVersionKind() == admissionCheckGVK {
		c.reads++
		return errors.New("admission check access denied")
	}
	return c.Client.Get(ctx, key, object, options...)
}

func validProfileDependencies(queueName string, namespaces ...string) []client.Object {
	objects := make([]client.Object, 0, len(namespaces)+6)
	for _, namespace := range namespaces {
		objects = append(objects, testLocalQueue(namespace, queueName, queueName+"-cq"))
	}
	objects = append(objects,
		testObservedClusterQueue(queueName+"-cq", []string{"h200", "a100"}, true),
		testResourceFlavor("h200", "gpu-topology"),
		testResourceFlavor("a100", ""),
		testKueueObject(topologyGVK, "gpu-topology"),
		testKueueObject(workloadPriorityClassGVK, queueName+"-priority"),
		&schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: queueName + "-pod-priority"}, Value: 100},
	)
	return objects
}

func testObservedClusterQueue(name string, flavors []string, active bool) *unstructured.Unstructured {
	flavorEntries := make([]any, 0, len(flavors))
	for _, flavor := range flavors {
		flavorEntries = append(flavorEntries, map[string]any{"name": flavor})
	}
	object := testKueueObject(clusterQueueGVK, name)
	object.Object["spec"] = map[string]any{"resourceGroups": []any{map[string]any{"flavors": flavorEntries}}}
	object.Object["status"] = map[string]any{"conditions": []any{map[string]any{"type": "Active", "status": map[bool]string{true: "True", false: "False"}[active]}}}
	return object
}

func testResourceFlavor(name, topology string) *unstructured.Unstructured {
	object := testKueueObject(resourceFlavorGVK, name)
	if topology != "" {
		object.Object["spec"] = map[string]any{"topologyName": topology}
	}
	return object
}

func testKueueObject(gvk schema.GroupVersionKind, name string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": gvk.GroupVersion().String(),
		"kind":       gvk.Kind,
		"metadata":   map[string]any{"name": name},
	}}
	object.SetGroupVersionKind(gvk)
	return object
}

func omitProfileDependency(objects []client.Object, omit string) []client.Object {
	out := make([]client.Object, 0, len(objects))
	for _, object := range objects {
		if objectKey(object) != omit {
			out = append(out, object)
		}
	}
	return out
}

func objectKey(object client.Object) string {
	switch typed := object.(type) {
	case *schedulingv1.PriorityClass:
		return "priorityclass/" + typed.Name
	case *unstructured.Unstructured:
		if typed.GetNamespace() != "" {
			return strings.ToLower(typed.GetKind()) + "/" + typed.GetNamespace() + "/" + typed.GetName()
		}
		return strings.ToLower(typed.GetKind()) + "/" + typed.GetName()
	default:
		return strings.ToLower(object.GetObjectKind().GroupVersionKind().Kind) + "/" + object.GetName()
	}
}

func uniqueProfileDependencies(objects []client.Object) []client.Object {
	seen := make(map[string]struct{}, len(objects))
	out := make([]client.Object, 0, len(objects))
	for _, object := range objects {
		key := objectKey(object)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, object)
	}
	return out
}
