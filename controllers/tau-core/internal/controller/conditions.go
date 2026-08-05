package controller

import (
	tauv1alpha1 "github.com/Azure/taugrid/controllers/tau-core/api/v1alpha1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func condition(conditionType string, status metav1.ConditionStatus, reason, message string, generation int64) metav1.Condition {
	return metav1.Condition{
		Type:               conditionType,
		Status:             status,
		ObservedGeneration: generation,
		LastTransitionTime: metav1.Now(),
		Reason:             reason,
		Message:            message,
	}
}

func boolCondition(conditionType string, ok bool, reason, message string, generation int64) metav1.Condition {
	if ok {
		return condition(conditionType, metav1.ConditionTrue, reason, message, generation)
	}
	return condition(conditionType, metav1.ConditionFalse, reason, message, generation)
}

func workspacePhase(conditions []metav1.Condition) string {
	required := map[string]metav1.ConditionStatus{
		tauv1alpha1.ConditionRBACReady:  metav1.ConditionUnknown,
		tauv1alpha1.ConditionQueueReady: metav1.ConditionUnknown,
	}
	for _, c := range conditions {
		if _, ok := required[c.Type]; ok {
			required[c.Type] = c.Status
		}
		if c.Type == tauv1alpha1.ConditionDriftDetected && c.Status == metav1.ConditionTrue {
			return tauv1alpha1.WorkspacePhaseDegraded
		}
	}
	pending := false
	for _, status := range required {
		switch status {
		case metav1.ConditionFalse:
			return tauv1alpha1.WorkspacePhaseDegraded
		case metav1.ConditionUnknown:
			pending = true
		}
	}
	if pending {
		return tauv1alpha1.WorkspacePhasePending
	}
	return tauv1alpha1.WorkspacePhaseReady
}

func mergeConditions(existing, desired []metav1.Condition) []metav1.Condition {
	byType := map[string]metav1.Condition{}
	for _, condition := range existing {
		byType[condition.Type] = condition
	}
	latest := map[string]metav1.Condition{}
	order := make([]string, 0, len(desired))
	for _, condition := range desired {
		if _, seen := latest[condition.Type]; !seen {
			order = append(order, condition.Type)
		}
		if previous, ok := byType[condition.Type]; ok &&
			previous.Status == condition.Status &&
			previous.Reason == condition.Reason &&
			previous.Message == condition.Message {
			condition.LastTransitionTime = previous.LastTransitionTime
		}
		latest[condition.Type] = condition
	}
	out := make([]metav1.Condition, 0, len(latest))
	for _, conditionType := range order {
		out = append(out, latest[conditionType])
	}
	return out
}

// equalWorkspaceStatus and equalQuotaRequestStatus gate the status write.
// mergeConditions already carries LastTransitionTime forward on an unchanged
// condition, so a semantic deep-equal is a faithful "nothing changed" test.
func equalWorkspaceStatus(a, b tauv1alpha1.TauWorkspaceStatus) bool {
	return apiequality.Semantic.DeepEqual(a, b)
}

func equalQuotaRequestStatus(a, b tauv1alpha1.TauQuotaRequestStatus) bool {
	return apiequality.Semantic.DeepEqual(a, b)
}

func equalTauClusterStatus(a, b tauv1alpha1.TauClusterStatus) bool {
	return apiequality.Semantic.DeepEqual(a, b)
}

// findCondition returns a pointer to the condition of the given type within
// conditions, or nil if not present.
func findCondition(conditions []metav1.Condition, conditionType string) *metav1.Condition {
	for i := range conditions {
		if conditions[i].Type == conditionType {
			return &conditions[i]
		}
	}
	return nil
}
