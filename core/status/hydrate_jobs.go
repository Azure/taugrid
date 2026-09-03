// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package status

import (
	"encoding/json"
	"strings"
	"time"
)

type jobObj struct {
	Metadata struct {
		UID               string            `json:"uid"`
		CreationTimestamp time.Time         `json:"creationTimestamp"`
		Labels            map[string]string `json:"labels"`
		Annotations       map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Suspend     *bool  `json:"suspend"`
		Parallelism *int   `json:"parallelism"`
		ManagedBy   string `json:"managedBy"`
	} `json:"spec"`
	Status struct {
		StartTime      time.Time `json:"startTime"`
		CompletionTime time.Time `json:"completionTime"`
		Active         int       `json:"active"`
		Succeeded      int       `json:"succeeded"`
		Failed         int       `json:"failed"`
		Conditions     []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

func hydrateJob(s *Snapshot, data []byte) {
	var j jobObj
	if err := json.Unmarshal(data, &j); err != nil {
		return
	}
	if j.Spec.Suspend != nil {
		s.JobSuspended = *j.Spec.Suspend
	}
	s.Labels = j.Metadata.Labels
	s.Annotations = j.Metadata.Annotations
	if j.Spec.Parallelism != nil {
		s.JobParallelism = *j.Spec.Parallelism
	} else {
		s.JobParallelism = 1
	}
	s.JobManagedBy = j.Spec.ManagedBy
	s.JobUID = j.Metadata.UID
	s.JobActive = j.Status.Active
	s.JobSucceeded = j.Status.Succeeded
	s.JobFailed = j.Status.Failed
	if !j.Metadata.CreationTimestamp.IsZero() {
		s.JobCreatedAt = j.Metadata.CreationTimestamp
		s.JobAgeSeconds = int64(time.Since(j.Metadata.CreationTimestamp).Seconds())
	}
	s.JobStartedAt = j.Status.StartTime
	s.JobFinishedAt = j.Status.CompletionTime
	for _, c := range j.Status.Conditions {
		s.JobConditions = append(s.JobConditions, Condition{
			Type: c.Type, Status: c.Status, Reason: c.Reason, Message: c.Message,
		})
	}
}

type rayJobObj struct {
	Metadata struct {
		UID               string            `json:"uid"`
		Name              string            `json:"name"`
		CreationTimestamp time.Time         `json:"creationTimestamp"`
		Labels            map[string]string `json:"labels"`
		Annotations       map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		ManagedBy string `json:"managedBy"`
	} `json:"spec"`
	Status struct {
		RayClusterName      string          `json:"rayClusterName"`
		JobID               string          `json:"jobId"`
		JobDeploymentStatus string          `json:"jobDeploymentStatus"`
		JobStatus           string          `json:"jobStatus"`
		RayClusterStatus    json.RawMessage `json:"rayClusterStatus"`
		StartTime           time.Time       `json:"startTime"`
		EndTime             time.Time       `json:"endTime"`
		Reason              string          `json:"reason"`
		Message             string          `json:"message"`
		Conditions          []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

func hydrateRayJob(s *Snapshot, data []byte) {
	var rj rayJobObj
	if err := json.Unmarshal(data, &rj); err != nil {
		return
	}
	s.RayJob = RayJob{
		Found:               true,
		Name:                firstNonEmpty(rj.Metadata.Name, s.Name),
		UID:                 rj.Metadata.UID,
		CreatedAt:           rj.Metadata.CreationTimestamp,
		StartedAt:           rj.Status.StartTime,
		FinishedAt:          rj.Status.EndTime,
		RayClusterName:      rj.Status.RayClusterName,
		JobID:               rj.Status.JobID,
		JobDeploymentStatus: rj.Status.JobDeploymentStatus,
		JobStatus:           rj.Status.JobStatus,
		RayClusterStatus:    summarizeRawStatus(rj.Status.RayClusterStatus),
		Reason:              rj.Status.Reason,
		Message:             rj.Status.Message,
		ManagedBy:           rj.Spec.ManagedBy,
	}
	s.RayJobFound = true
	s.RayJobStatus = rj.Status.JobDeploymentStatus
	s.RayJobReason = firstNonEmpty(rj.Status.Reason, rj.Status.Message)
	s.RayClusterName = rj.Status.RayClusterName
	s.RayJobID = rj.Status.JobID
	s.RayJobStartedAt = rj.Status.StartTime
	s.RayJobFinishedAt = rj.Status.EndTime
	s.RayJobCreatedAt = rj.Metadata.CreationTimestamp
	for _, c := range rj.Status.Conditions {
		s.RayJob.Conditions = append(s.RayJob.Conditions, Condition{
			Type: c.Type, Status: c.Status, Reason: c.Reason, Message: c.Message,
		})
	}
	if !s.JobFound && len(s.Labels) == 0 {
		s.Labels = rj.Metadata.Labels
	}
	if !s.JobFound && len(s.Annotations) == 0 {
		s.Annotations = rj.Metadata.Annotations
	}
}

func summarizeRawStatus(raw json.RawMessage) string {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" {
		return ""
	}
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		return value
	}
	var object map[string]any
	if err := json.Unmarshal(raw, &object); err != nil {
		return ""
	}
	for _, key := range []string{"state", "phase", "status", "reason"} {
		if s := stringValue(object[key]); s != "" {
			return s
		}
	}
	return "reported"
}
