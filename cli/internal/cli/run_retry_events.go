// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Azure/taugrid/cli/internal/resume"
	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/status"
)

const (
	retryEventReasonAttempt   = "RayJobAttempt"
	retryEventReasonExhausted = "RayJobRetriesExhausted"
	retryEventReasonSucceeded = "RayJobRetrySucceeded"
	retryEventReporter        = "tau-cli"
)

type retryEventObjectRef struct {
	apiVersion string
	kind       string
	name       string
}

func emitRetryEvent(ctx context.Context, r *kube.Runner, namespace, name string, attempt int, reason, message string) error {
	eventName := retryEventName(name, attempt, reason)
	ref := retryEventObject(ctx, r, namespace, name)
	body := renderRetryEventYAML(namespace, eventName, ref, reason, message, time.Now().UTC())
	_, err := r.Raw(ctx, []string{"-n", namespace, "apply", "-f", "-"}, body)
	return err
}

func retryEventObject(ctx context.Context, r *kube.Runner, namespace, name string) retryEventObjectRef {
	if _, err := r.Raw(ctx, []string{"-n", namespace, "get", "rayjob", name, "-o", "name"}, nil); err == nil {
		return retryEventObjectRef{apiVersion: "ray.io/v1", kind: "RayJob", name: name}
	}
	return retryEventObjectRef{apiVersion: "batch/v1", kind: "Job", name: name}
}

func renderRetryEventYAML(namespace, eventName string, ref retryEventObjectRef, reason, message string, ts time.Time) []byte {
	timestamp := ts.UTC().Format(time.RFC3339)
	return []byte(fmt.Sprintf(`apiVersion: v1
kind: Event
metadata:
  name: %s
  namespace: %s
involvedObject:
  apiVersion: %s
  kind: %s
  name: %s
  namespace: %s
reason: %s
message: %q
type: Normal
action: Retry
reportingComponent: %s
reportingInstance: %s
firstTimestamp: %q
lastTimestamp: %q
count: 1
source:
  component: %s
`, eventName, namespace, ref.apiVersion, ref.kind, ref.name, namespace, reason, message, retryEventReporter, retryEventReporter, timestamp, timestamp, retryEventReporter))
}

func retryEventName(name string, attempt int, reason string) string {
	suffix := "retry"
	switch reason {
	case retryEventReasonExhausted:
		suffix = "retries-exhausted"
	case retryEventReasonSucceeded:
		suffix = "retry-succeeded"
	}
	return fmt.Sprintf("%s-%s-%d", name, suffix, attempt)
}

func retryAttemptEventMessage(submissionID string, attempt, maxRetries int, failureReason, failureSignature string) string {
	return fmt.Sprintf("submission_id=%s attempt=%d/%d failure=%s signature=%s",
		submissionID,
		attempt,
		maxRetries,
		failureReason,
		retryEventFieldValue(failureSignature),
	)
}

func retryExhaustedEventMessage(submissionID string, maxRetries int, failureReason, failureSignature string) string {
	return fmt.Sprintf("submission_id=%s retries_exhausted=%d final_failure=%s signature=%s",
		submissionID,
		maxRetries,
		failureReason,
		retryEventFieldValue(failureSignature),
	)
}

func retrySuccessEventMessage(submissionID string, attempt int, failureReason, failureSignature string) string {
	return fmt.Sprintf("submission_id=%s recovered_after_attempt=%d last_failure=%s signature=%s",
		submissionID,
		attempt,
		retryEventFieldValue(failureReason),
		retryEventFieldValue(failureSignature),
	)
}

func retryFailureSignature(snap status.Snapshot, reason resume.FailureReason) string {
	rj := snap.RayJob
	if !rj.Found && snap.RayJobFound {
		rj = status.RayJob{
			Found:               true,
			Name:                snap.Name,
			JobDeploymentStatus: snap.RayJobStatus,
			Reason:              snap.RayJobReason,
		}
	}
	if msg := firstNonEmpty(rj.Message, rj.Reason); msg != "" {
		return retryTruncateMessage(msg)
	}
	for _, p := range snap.Pods {
		if p.OOMKilled {
			return "pod=" + p.Name + " oom_killed=true"
		}
		if msg := firstNonEmpty(p.ContainerReason, podConditionMessage(p.Conditions)); msg != "" {
			return retryTruncateMessage("pod=" + p.Name + " " + msg)
		}
		for _, c := range append(append([]status.Container{}, p.InitContainers...), p.Containers...) {
			if msg := firstNonEmpty(c.Reason, c.LastReason, c.Message, c.LastMessage); msg != "" {
				return retryTruncateMessage("pod=" + p.Name + " container=" + c.Name + " " + msg)
			}
		}
	}
	for _, ev := range snap.Events {
		if msg := firstNonEmpty(ev.Reason, ev.Message); msg != "" {
			return retryTruncateMessage(msg)
		}
	}
	return reason.String()
}

func podConditionMessage(conditions []status.Condition) string {
	for _, cond := range conditions {
		if cond.Status == "True" || cond.Status == "False" {
			if msg := firstNonEmpty(cond.Reason, cond.Message, cond.Type); msg != "" {
				return msg
			}
		}
	}
	return ""
}

func retryTruncateMessage(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	const maxLen = 160
	if len(message) <= maxLen {
		return message
	}
	return message[:maxLen-3] + "..."
}

func retryEventFieldValue(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}
