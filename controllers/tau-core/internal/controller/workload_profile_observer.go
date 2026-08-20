// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	profile "github.com/Azure/taugrid/core/resourceprofile"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const multiKueueAdmissionCheckController = "kueue.x-k8s.io/multikueue"

type profileObservationState struct {
	status    profile.ProfileSetStatus
	condition metav1.Condition
}

func (r *TauClusterReconciler) observeWorkloadProfiles(
	ctx context.Context,
	cluster *tauv1alpha1.TauCluster,
) (profileObservationState, error) {
	generation := cluster.Generation
	state := profileObservationState{
		status: profile.ProfileSetStatus{
			ObservedGeneration: generation,
			Observed:           int32(len(cluster.Spec.WorkloadProfiles)),
			Profiles:           []profile.ResolvedWorkloadProfile{},
		},
	}
	var observationErr error
	for _, declared := range cluster.Spec.WorkloadProfiles {
		resolved, err := r.observeWorkloadProfile(ctx, cluster, declared)
		observationErr = errors.Join(observationErr, err)
		if previous := previousResolvedProfile(cluster.Status.WorkloadProfiles.Profiles, resolved.Name); previous != nil {
			resolved.Conditions = mergeConditions(previous.Conditions, resolved.Conditions)
		}
		if readyCondition := findCondition(resolved.Conditions, profile.ConditionReady); readyCondition != nil &&
			readyCondition.Status == metav1.ConditionTrue {
			state.status.Ready++
		} else {
			state.status.Drifted++
		}
		state.status.Profiles = append(state.status.Profiles, resolved)
	}
	sort.Slice(state.status.Profiles, func(i, j int) bool {
		return state.status.Profiles[i].Name < state.status.Profiles[j].Name
	})

	hash, hashErr := profile.ProfileSetHash(state.status.Profiles)
	if hashErr == nil {
		state.status.ProfileSetHash = hash
	}

	switch {
	case state.status.Observed == 0:
		state.condition = condition(
			tauv1alpha1.ConditionWorkloadProfilesReady,
			metav1.ConditionTrue,
			"NoWorkloadProfiles",
			"no workload profiles are declared",
			generation,
		)
	case state.status.Drifted == 0:
		state.condition = condition(
			tauv1alpha1.ConditionWorkloadProfilesReady,
			metav1.ConditionTrue,
			"WorkloadProfilesReady",
			fmt.Sprintf("%d workload profiles resolved successfully", state.status.Ready),
			generation,
		)
	default:
		state.condition = condition(
			tauv1alpha1.ConditionWorkloadProfilesReady,
			metav1.ConditionFalse,
			"WorkloadProfilesNotReady",
			fmt.Sprintf("%d of %d workload profiles are not ready", state.status.Drifted, state.status.Observed),
			generation,
		)
	}
	return state, observationErr
}

func (r *TauClusterReconciler) observeWorkloadProfile(
	ctx context.Context,
	cluster *tauv1alpha1.TauCluster,
	declared profile.WorkloadProfile,
) (profile.ResolvedWorkloadProfile, error) {
	generation := cluster.Generation
	normalized := profile.NormalizeWorkloadProfile(declared)
	resolved := profile.ResolvedWorkloadProfile{WorkloadProfile: normalized}
	if err := profile.ValidateWorkloadProfile(normalized); err != nil {
		message := fmt.Sprintf("invalid workload profile %q: %v", normalized.Name, err)
		resolved.Conditions = []metav1.Condition{
			condition(profile.ConditionLocalQueuesResolved, metav1.ConditionUnknown, "ValidationSkipped", message, generation),
			condition(profile.ConditionClusterQueuesReady, metav1.ConditionUnknown, "ValidationSkipped", message, generation),
			condition(profile.ConditionResourceFlavorsReady, metav1.ConditionUnknown, "ValidationSkipped", message, generation),
			condition(profile.ConditionTopologiesReady, metav1.ConditionUnknown, "ValidationSkipped", message, generation),
			condition(profile.ConditionPriorityClassesReady, metav1.ConditionUnknown, "ValidationSkipped", message, generation),
			condition(profile.ConditionExecutionReady, metav1.ConditionUnknown, "ValidationSkipped", message, generation),
			condition(profile.ConditionReady, metav1.ConditionFalse, "InvalidWorkloadProfile", message, generation),
		}
		return resolved, nil
	}

	namespaces := applicableQueueNamespaces(cluster, normalized)
	localOK := len(namespaces) > 0
	localIssues := make([]string, 0)
	if !localOK {
		localIssues = append(localIssues, fmt.Sprintf(
			"profile has global namespace applicability but spec.queues.sharedLocalQueues declares no %q LocalQueue",
			normalized.DefaultLocalQueue,
		))
	}
	clusterQueueNames := make(map[string]struct{})
	var observationErr error
	for _, namespace := range namespaces {
		localQueue := newQueueObject(localQueueGVK)
		key := client.ObjectKey{Namespace: namespace, Name: normalized.DefaultLocalQueue}
		if err := r.Get(ctx, key, localQueue); err != nil {
			localOK = false
			if apierrors.IsNotFound(err) {
				localIssues = append(localIssues, fmt.Sprintf("LocalQueue %s/%s does not exist", namespace, normalized.DefaultLocalQueue))
			} else {
				localIssues = append(localIssues, fmt.Sprintf("cannot read LocalQueue %s/%s: %v", namespace, normalized.DefaultLocalQueue, err))
				observationErr = errors.Join(observationErr, fmt.Errorf("read LocalQueue %s/%s: %w", namespace, normalized.DefaultLocalQueue, err))
			}
			continue
		}
		clusterQueueName, err := requiredNestedString(localQueue, "spec", "clusterQueue")
		if err != nil {
			localOK = false
			localIssues = append(localIssues, fmt.Sprintf("LocalQueue %s/%s is malformed: %v", namespace, normalized.DefaultLocalQueue, err))
			continue
		}
		resolved.LocalQueues = append(resolved.LocalQueues, profile.ResolvedLocalQueue{
			Namespace: namespace, Name: normalized.DefaultLocalQueue, ClusterQueue: clusterQueueName,
		})
		clusterQueueNames[clusterQueueName] = struct{}{}
	}
	resolved.Conditions = append(resolved.Conditions, observationCondition(
		profile.ConditionLocalQueuesResolved,
		localOK,
		"LocalQueuesResolved",
		"LocalQueuesNotResolved",
		fmt.Sprintf("%d applicable LocalQueues resolved", len(resolved.LocalQueues)),
		localIssues,
		generation,
	))

	clusterQueueOK := localOK
	clusterQueueIssues := make([]string, 0)
	flavorNames := make(map[string]struct{})
	executionObservations := make([]clusterQueueExecutionObservation, 0, len(clusterQueueNames))
	classifyAdmissionChecks := cluster.Spec.Features.MultiKueue == tauv1alpha1.TauClusterFeatureBeta &&
		r.MultiKueueBetaRuntimeEnabled
	for _, name := range sortedSet(clusterQueueNames) {
		clusterQueue := newQueueObject(clusterQueueGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: name}, clusterQueue); err != nil {
			clusterQueueOK = false
			if apierrors.IsNotFound(err) {
				clusterQueueIssues = append(clusterQueueIssues, fmt.Sprintf("ClusterQueue %s does not exist", name))
			} else {
				clusterQueueIssues = append(clusterQueueIssues, fmt.Sprintf("cannot read ClusterQueue %s: %v", name, err))
				observationErr = errors.Join(observationErr, fmt.Errorf("read ClusterQueue %s: %w", name, err))
			}
			continue
		}
		resolved.ClusterQueues = append(resolved.ClusterQueues, name)
		executionObservation, err := r.observeClusterQueueExecution(
			ctx,
			name,
			clusterQueue,
			classifyAdmissionChecks,
		)
		executionObservations = append(executionObservations, executionObservation)
		observationErr = errors.Join(observationErr, err)
		active, detail, err := clusterQueueActive(clusterQueue)
		if err != nil {
			clusterQueueOK = false
			clusterQueueIssues = append(clusterQueueIssues, fmt.Sprintf("ClusterQueue %s has malformed status: %v", name, err))
		} else if !active {
			clusterQueueOK = false
			clusterQueueIssues = append(clusterQueueIssues, fmt.Sprintf("ClusterQueue %s is inactive%s", name, detail))
		}
		names, err := clusterQueueFlavorNames(clusterQueue)
		if err != nil {
			clusterQueueOK = false
			clusterQueueIssues = append(clusterQueueIssues, fmt.Sprintf("ClusterQueue %s has malformed resource flavors: %v", name, err))
			continue
		}
		for _, flavorName := range names {
			flavorNames[flavorName] = struct{}{}
		}
	}
	resolved.Conditions = append(resolved.Conditions, observationCondition(
		profile.ConditionClusterQueuesReady,
		clusterQueueOK,
		"ClusterQueuesReady",
		"ClusterQueuesNotReady",
		fmt.Sprintf("%d ClusterQueues are readable and active", len(resolved.ClusterQueues)),
		clusterQueueIssues,
		generation,
	))
	executionCondition := workloadProfileExecutionCondition(
		cluster,
		normalized,
		clusterQueueOK,
		len(clusterQueueNames),
		executionObservations,
		classifyAdmissionChecks,
		generation,
	)
	resolved.Conditions = append(resolved.Conditions, executionCondition)

	flavorsOK := clusterQueueOK
	flavorIssues := make([]string, 0)
	topologyNames := make(map[string]struct{})
	for _, name := range sortedSet(flavorNames) {
		flavor := newQueueObject(resourceFlavorGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: name}, flavor); err != nil {
			flavorsOK = false
			if apierrors.IsNotFound(err) {
				flavorIssues = append(flavorIssues, fmt.Sprintf("ResourceFlavor %s does not exist", name))
			} else {
				flavorIssues = append(flavorIssues, fmt.Sprintf("cannot read ResourceFlavor %s: %v", name, err))
				observationErr = errors.Join(observationErr, fmt.Errorf("read ResourceFlavor %s: %w", name, err))
			}
			continue
		}
		resolved.ResourceFlavors = append(resolved.ResourceFlavors, name)
		topologyName, err := optionalNestedString(flavor, "spec", "topologyName")
		if err != nil {
			flavorsOK = false
			flavorIssues = append(flavorIssues, fmt.Sprintf("ResourceFlavor %s has malformed topologyName: %v", name, err))
			continue
		}
		if topologyName != "" {
			topologyNames[topologyName] = struct{}{}
		}
	}
	resolved.Conditions = append(resolved.Conditions, observationCondition(
		profile.ConditionResourceFlavorsReady,
		flavorsOK,
		"ResourceFlavorsReady",
		"ResourceFlavorsNotReady",
		fmt.Sprintf("%d ResourceFlavors are readable", len(resolved.ResourceFlavors)),
		flavorIssues,
		generation,
	))

	topologiesOK := flavorsOK
	topologyIssues := make([]string, 0)
	for _, name := range sortedSet(topologyNames) {
		topology := newQueueObject(topologyGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: name}, topology); err != nil {
			topologiesOK = false
			if apierrors.IsNotFound(err) {
				topologyIssues = append(topologyIssues, fmt.Sprintf("Topology %s does not exist", name))
			} else {
				topologyIssues = append(topologyIssues, fmt.Sprintf("cannot read Topology %s: %v", name, err))
				observationErr = errors.Join(observationErr, fmt.Errorf("read Topology %s: %w", name, err))
			}
			continue
		}
		resolved.Topologies = append(resolved.Topologies, name)
	}
	resolved.Conditions = append(resolved.Conditions, observationCondition(
		profile.ConditionTopologiesReady,
		topologiesOK,
		"TopologiesReady",
		"TopologiesNotReady",
		fmt.Sprintf("%d Topologies are readable", len(resolved.Topologies)),
		topologyIssues,
		generation,
	))

	prioritiesOK := true
	priorityIssues := make([]string, 0)
	if !normalized.Priorities.DisableDefaultPriorities {
		workloadPriority := newQueueObject(workloadPriorityClassGVK)
		workloadName := normalized.Priorities.WorkloadPriorityClassName
		if err := r.Get(ctx, client.ObjectKey{Name: workloadName}, workloadPriority); err != nil {
			prioritiesOK = false
			if apierrors.IsNotFound(err) {
				priorityIssues = append(priorityIssues, fmt.Sprintf("WorkloadPriorityClass %s does not exist", workloadName))
			} else {
				priorityIssues = append(priorityIssues, fmt.Sprintf("cannot read WorkloadPriorityClass %s: %v", workloadName, err))
				observationErr = errors.Join(observationErr, fmt.Errorf("read WorkloadPriorityClass %s: %w", workloadName, err))
			}
		} else {
			resolved.WorkloadPriorityClasses = []string{workloadName}
		}

		var podPriority schedulingv1.PriorityClass
		podName := normalized.Priorities.PodPriorityClassName
		if err := r.Get(ctx, client.ObjectKey{Name: podName}, &podPriority); err != nil {
			prioritiesOK = false
			if apierrors.IsNotFound(err) {
				priorityIssues = append(priorityIssues, fmt.Sprintf("PriorityClass %s does not exist", podName))
			} else {
				priorityIssues = append(priorityIssues, fmt.Sprintf("cannot read PriorityClass %s: %v", podName, err))
				observationErr = errors.Join(observationErr, fmt.Errorf("read PriorityClass %s: %w", podName, err))
			}
		} else {
			resolved.PodPriorityClasses = []string{podName}
		}
	}
	resolved.Conditions = append(resolved.Conditions, observationCondition(
		profile.ConditionPriorityClassesReady,
		prioritiesOK,
		"PriorityClassesReady",
		"PriorityClassesNotReady",
		"referenced priority classes are readable",
		priorityIssues,
		generation,
	))

	ready := localOK && clusterQueueOK && flavorsOK && topologiesOK && prioritiesOK &&
		executionCondition.Status == metav1.ConditionTrue
	readyMessage := "all workload profile dependencies are ready"
	if !ready {
		readyMessage = "one or more workload profile dependencies are not ready"
	}
	resolved.Conditions = append(resolved.Conditions, boolCondition(
		profile.ConditionReady,
		ready,
		map[bool]string{true: "WorkloadProfileReady", false: "WorkloadProfileNotReady"}[ready],
		readyMessage,
		generation,
	))
	return resolved, observationErr
}

type clusterQueueExecutionObservation struct {
	clusterQueue              string
	referencedAdmissionChecks []string
	multiKueueChecks          []string
	otherChecks               []string
	issues                    []string
}

func (o clusterQueueExecutionObservation) usesOnlyMultiKueue() bool {
	return len(o.referencedAdmissionChecks) > 0 &&
		len(o.multiKueueChecks) == len(o.referencedAdmissionChecks) &&
		len(o.issues) == 0
}

func (r *TauClusterReconciler) observeClusterQueueExecution(
	ctx context.Context,
	clusterQueueName string,
	clusterQueue *unstructured.Unstructured,
	classifyAdmissionChecks bool,
) (clusterQueueExecutionObservation, error) {
	observation := clusterQueueExecutionObservation{clusterQueue: clusterQueueName}
	names, err := clusterQueueAdmissionCheckNames(clusterQueue)
	if err != nil {
		observation.issues = append(observation.issues, fmt.Sprintf(
			"ClusterQueue %s has malformed admission-check references: %v",
			clusterQueueName,
			err,
		))
		return observation, nil
	}
	observation.referencedAdmissionChecks = names
	if !classifyAdmissionChecks {
		return observation, nil
	}

	var observationErr error
	for _, name := range names {
		admissionCheck := newQueueObject(admissionCheckGVK)
		if err := r.Get(ctx, client.ObjectKey{Name: name}, admissionCheck); err != nil {
			if apierrors.IsNotFound(err) {
				observation.issues = append(observation.issues, fmt.Sprintf(
					"AdmissionCheck %s referenced by ClusterQueue %s does not exist",
					name,
					clusterQueueName,
				))
			} else {
				observation.issues = append(observation.issues, fmt.Sprintf(
					"cannot read AdmissionCheck %s referenced by ClusterQueue %s: %v",
					name,
					clusterQueueName,
					err,
				))
				observationErr = errors.Join(observationErr, fmt.Errorf("read AdmissionCheck %s: %w", name, err))
			}
			continue
		}
		controllerName, err := admissionCheckControllerName(admissionCheck)
		if err != nil {
			observation.issues = append(observation.issues, fmt.Sprintf(
				"AdmissionCheck %s referenced by ClusterQueue %s is malformed: %v",
				name,
				clusterQueueName,
				err,
			))
			continue
		}
		if controllerName == multiKueueAdmissionCheckController {
			observation.multiKueueChecks = append(observation.multiKueueChecks, name)
			ready, issue, err := (&KubernetesMultiKueuePrerequisites{Reader: r.Client}).
				admissionCheckReady(ctx, admissionCheck)
			if err != nil {
				observationErr = errors.Join(observationErr, fmt.Errorf(
					"validate AdmissionCheck %s prerequisites: %w",
					name,
					err,
				))
			}
			if !ready {
				observation.issues = append(observation.issues, issue)
			}
		} else {
			observation.otherChecks = append(observation.otherChecks, name)
		}
	}
	return observation, observationErr
}

func workloadProfileExecutionCondition(
	cluster *tauv1alpha1.TauCluster,
	workloadProfile profile.WorkloadProfile,
	clusterQueuesReady bool,
	referencedClusterQueues int,
	observations []clusterQueueExecutionObservation,
	classifyAdmissionChecks bool,
	generation int64,
) metav1.Condition {
	var lookupIssues []string
	var multiKueueChecks []string
	for _, observation := range observations {
		lookupIssues = append(lookupIssues, observation.issues...)
		multiKueueChecks = append(multiKueueChecks, observation.multiKueueChecks...)
	}
	if workloadProfile.ExecutionTarget == profile.ExecutionTargetMultiKueueBeta {
		if notReady := multiKueueCapabilityNotReadyCondition(cluster, generation); notReady != nil {
			return *notReady
		}
	}
	if len(lookupIssues) > 0 {
		sort.Strings(lookupIssues)
		return condition(
			profile.ConditionExecutionReady,
			metav1.ConditionFalse,
			"AdmissionChecksNotReady",
			strings.Join(lookupIssues, "; "),
			generation,
		)
	}

	if workloadProfile.ExecutionTarget == profile.ExecutionTargetSingleCluster {
		if !classifyAdmissionChecks {
			return condition(
				profile.ConditionExecutionReady,
				metav1.ConditionTrue,
				"Ready",
				"ordinary profile dependencies are ready; MultiKueue classification is disabled",
				generation,
			)
		}
		if len(multiKueueChecks) > 0 {
			sort.Strings(multiKueueChecks)
			return condition(
				profile.ConditionExecutionReady,
				metav1.ConditionFalse,
				"UnexpectedMultiKueueAdmissionCheck",
				fmt.Sprintf(
					"singleCluster profile resolves to MultiKueue AdmissionCheck(s) %s; set executionTarget=multiKueueBeta and explicit team/namespace allowlists or remove the wiring",
					strings.Join(multiKueueChecks, ", "),
				),
				generation,
			)
		}
		return condition(
			profile.ConditionExecutionReady,
			metav1.ConditionTrue,
			"Ready",
			"resolved ClusterQueues do not use MultiKueue",
			generation,
		)
	}

	defaultQueue := strings.TrimSpace(cluster.Spec.WorkspaceDefaults.DefaultQueue)
	if defaultQueue == "" {
		defaultQueue = "jobqueue"
	}
	if strings.EqualFold(defaultQueue, workloadProfile.DefaultLocalQueue) {
		return condition(
			profile.ConditionExecutionReady,
			metav1.ConditionFalse,
			"DefaultQueueNotAllowed",
			fmt.Sprintf(
				"multiKueueBeta profile queue %q must not be TauCluster workspaceDefaults.defaultQueue",
				workloadProfile.DefaultLocalQueue,
			),
			generation,
		)
	}

	wiringIssues := make([]string, 0)
	if !clusterQueuesReady || len(observations) != referencedClusterQueues || referencedClusterQueues == 0 {
		wiringIssues = append(wiringIssues, "all referenced ClusterQueues must be readable and active")
	}
	for _, observation := range observations {
		switch {
		case len(observation.referencedAdmissionChecks) == 0:
			wiringIssues = append(wiringIssues, fmt.Sprintf(
				"ClusterQueue %s has no admission checks",
				observation.clusterQueue,
			))
		case !observation.usesOnlyMultiKueue():
			wiringIssues = append(wiringIssues, fmt.Sprintf(
				"ClusterQueue %s does not use only MultiKueue AdmissionChecks; non-MultiKueue checks: %s",
				observation.clusterQueue,
				strings.Join(observation.otherChecks, ", "),
			))
		}
	}
	if len(wiringIssues) > 0 {
		sort.Strings(wiringIssues)
		return condition(
			profile.ConditionExecutionReady,
			metav1.ConditionFalse,
			"MultiKueueWiringNotReady",
			strings.Join(wiringIssues, "; "),
			generation,
		)
	}

	return condition(
		profile.ConditionExecutionReady,
		metav1.ConditionTrue,
		"Ready",
		"all ClusterQueues use MultiKueue and the TauCluster capability is current and ready",
		generation,
	)
}

func multiKueueCapabilityNotReadyCondition(
	cluster *tauv1alpha1.TauCluster,
	generation int64,
) *metav1.Condition {
	capability := findCondition(cluster.Status.Conditions, tauv1alpha1.ConditionMultiKueueReady)
	if capability == nil {
		result := condition(
			profile.ConditionExecutionReady,
			metav1.ConditionFalse,
			"PrerequisitesNotReady",
			"TauCluster has no MultiKueueReady capability condition",
			generation,
		)
		return &result
	}
	if capability.ObservedGeneration != generation {
		result := condition(
			profile.ConditionExecutionReady,
			metav1.ConditionFalse,
			"PrerequisitesNotReady",
			fmt.Sprintf(
				"TauCluster MultiKueueReady condition observed generation %d, want current generation %d",
				capability.ObservedGeneration,
				generation,
			),
			generation,
		)
		return &result
	}
	if capability.Status != metav1.ConditionTrue {
		reason := capability.Reason
		if reason == "" {
			reason = "PrerequisitesNotReady"
		}
		message := strings.TrimSpace(capability.Message)
		if message == "" {
			message = "TauCluster MultiKueue capability is not ready"
		}
		result := condition(profile.ConditionExecutionReady, metav1.ConditionFalse, reason, message, generation)
		return &result
	}
	return nil
}

func applicableQueueNamespaces(cluster *tauv1alpha1.TauCluster, workloadProfile profile.WorkloadProfile) []string {
	if len(workloadProfile.Applicability.Namespaces) > 0 {
		return append([]string(nil), workloadProfile.Applicability.Namespaces...)
	}
	namespaces := make(map[string]struct{})
	for _, ref := range cluster.Spec.Queues.SharedLocalQueues {
		if strings.EqualFold(strings.TrimSpace(ref.Name), workloadProfile.DefaultLocalQueue) {
			namespace := strings.ToLower(strings.TrimSpace(ref.Namespace))
			if namespace != "" {
				namespaces[namespace] = struct{}{}
			}
		}
	}
	return sortedSet(namespaces)
}

func previousResolvedProfile(profiles []profile.ResolvedWorkloadProfile, name string) *profile.ResolvedWorkloadProfile {
	for i := range profiles {
		if profiles[i].Name == name {
			return &profiles[i]
		}
	}
	return nil
}

func observationCondition(
	conditionType string,
	ok bool,
	readyReason string,
	notReadyReason string,
	readyMessage string,
	issues []string,
	generation int64,
) metav1.Condition {
	if ok {
		return condition(conditionType, metav1.ConditionTrue, readyReason, readyMessage, generation)
	}
	sort.Strings(issues)
	return condition(conditionType, metav1.ConditionFalse, notReadyReason, strings.Join(issues, "; "), generation)
}

func requiredNestedString(obj *unstructured.Unstructured, fields ...string) (string, error) {
	value, found, err := unstructured.NestedString(obj.Object, fields...)
	if err != nil {
		return "", err
	}
	if !found || strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("%s is required", strings.Join(fields, "."))
	}
	return strings.TrimSpace(value), nil
}

func optionalNestedString(obj *unstructured.Unstructured, fields ...string) (string, error) {
	value, found, err := unstructured.NestedString(obj.Object, fields...)
	if err != nil {
		return "", err
	}
	if !found {
		return "", nil
	}
	return strings.TrimSpace(value), nil
}

func clusterQueueFlavorNames(clusterQueue *unstructured.Unstructured) ([]string, error) {
	resourceGroups, found, err := unstructured.NestedSlice(clusterQueue.Object, "spec", "resourceGroups")
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	names := make(map[string]struct{})
	for groupIndex, rawGroup := range resourceGroups {
		group, ok := rawGroup.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("spec.resourceGroups[%d] must be an object", groupIndex)
		}
		rawFlavors, found, err := unstructured.NestedSlice(group, "flavors")
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		for flavorIndex, rawFlavor := range rawFlavors {
			flavor, ok := rawFlavor.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("spec.resourceGroups[%d].flavors[%d] must be an object", groupIndex, flavorIndex)
			}
			name, found, err := unstructured.NestedString(flavor, "name")
			if err != nil {
				return nil, err
			}
			if !found || strings.TrimSpace(name) == "" {
				return nil, fmt.Errorf("spec.resourceGroups[%d].flavors[%d].name is required", groupIndex, flavorIndex)
			}
			names[strings.TrimSpace(name)] = struct{}{}
		}
	}
	return sortedSet(names), nil
}

// clusterQueueAdmissionCheckNames accepts the legacy v1beta1 string list and
// the v1beta2 admissionChecksStrategy rule list.
func clusterQueueAdmissionCheckNames(clusterQueue *unstructured.Unstructured) ([]string, error) {
	legacyChecks, legacyFound, err := unstructured.NestedSlice(clusterQueue.Object, "spec", "admissionChecks")
	if err != nil {
		return nil, err
	}
	strategyChecks, strategyFound, err := unstructured.NestedSlice(
		clusterQueue.Object,
		"spec",
		"admissionChecksStrategy",
		"admissionChecks",
	)
	if err != nil {
		return nil, err
	}
	if legacyFound && strategyFound {
		return nil, errors.New("spec.admissionChecks and spec.admissionChecksStrategy cannot both be set")
	}
	if !legacyFound && !strategyFound {
		return nil, nil
	}
	rawChecks := legacyChecks
	if strategyFound {
		rawChecks = strategyChecks
	}
	names := make([]string, 0, len(rawChecks))
	seen := make(map[string]struct{}, len(rawChecks))
	for index, rawCheck := range rawChecks {
		var name string
		if strategyFound {
			check, ok := rawCheck.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("spec.admissionChecksStrategy.admissionChecks[%d] must be an object", index)
			}
			value, found, err := unstructured.NestedString(check, "name")
			if err != nil {
				return nil, fmt.Errorf("spec.admissionChecksStrategy.admissionChecks[%d].name: %w", index, err)
			}
			if !found {
				return nil, fmt.Errorf("spec.admissionChecksStrategy.admissionChecks[%d].name is required", index)
			}
			name = strings.TrimSpace(value)
		} else {
			value, ok := rawCheck.(string)
			if !ok {
				return nil, fmt.Errorf("spec.admissionChecks[%d] must be a string", index)
			}
			name = strings.TrimSpace(value)
		}
		if name == "" {
			return nil, fmt.Errorf("admission check at index %d has an empty name", index)
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("spec.admissionChecks contains duplicate name %q", name)
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

func admissionCheckControllerName(admissionCheck *unstructured.Unstructured) (string, error) {
	controllerName, found, err := unstructured.NestedString(admissionCheck.Object, "spec", "controllerName")
	if err != nil {
		return "", err
	}
	if !found || strings.TrimSpace(controllerName) == "" {
		return "", errors.New("spec.controllerName is required")
	}
	return controllerName, nil
}

func clusterQueueActive(clusterQueue *unstructured.Unstructured) (bool, string, error) {
	conditions, found, err := unstructured.NestedSlice(clusterQueue.Object, "status", "conditions")
	if err != nil {
		return false, "", err
	}
	if !found {
		return false, ": status.conditions is not reported", nil
	}
	for index, rawCondition := range conditions {
		conditionMap, ok := rawCondition.(map[string]any)
		if !ok {
			return false, "", fmt.Errorf("status.conditions[%d] must be an object", index)
		}
		conditionType, _, err := unstructured.NestedString(conditionMap, "type")
		if err != nil {
			return false, "", err
		}
		if conditionType != "Active" {
			continue
		}
		status, found, err := unstructured.NestedString(conditionMap, "status")
		if err != nil {
			return false, "", err
		}
		if !found {
			return false, "", fmt.Errorf("status.conditions[%d].status is required", index)
		}
		if status == string(metav1.ConditionTrue) {
			return true, "", nil
		}
		reason, _, _ := unstructured.NestedString(conditionMap, "reason")
		message, _, _ := unstructured.NestedString(conditionMap, "message")
		detailParts := make([]string, 0, 2)
		for _, part := range []string{reason, message} {
			if part = strings.TrimSpace(part); part != "" {
				detailParts = append(detailParts, part)
			}
		}
		detail := strings.Join(detailParts, ": ")
		if detail != "" {
			detail = ": " + detail
		}
		return false, detail, nil
	}
	return false, ": Active condition is not reported", nil
}

func sortedSet(values map[string]struct{}) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
