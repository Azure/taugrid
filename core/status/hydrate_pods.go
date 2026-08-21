// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package status

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

type podList struct {
	Items []struct {
		Metadata struct {
			Name              string    `json:"name"`
			UID               string    `json:"uid"`
			CreationTimestamp time.Time `json:"creationTimestamp"`
		} `json:"metadata"`
		Spec struct {
			NodeName       string `json:"nodeName"`
			ResourceClaims []struct {
				Name                      string `json:"name"`
				ResourceClaimName         string `json:"resourceClaimName"`
				ResourceClaimTemplateName string `json:"resourceClaimTemplateName"`
			} `json:"resourceClaims"`
		} `json:"spec"`
		Status struct {
			StartTime  time.Time `json:"startTime"`
			Phase      string    `json:"phase"`
			Conditions []struct {
				Type    string `json:"type"`
				Status  string `json:"status"`
				Reason  string `json:"reason"`
				Message string `json:"message"`
			} `json:"conditions"`
			ResourceClaimStatuses []struct {
				Name              string `json:"name"`
				ResourceClaimName string `json:"resourceClaimName"`
			} `json:"resourceClaimStatuses"`
			InitContainerStatuses []containerStatusObj `json:"initContainerStatuses"`
			ContainerStatuses     []containerStatusObj `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

type containerStatusObj struct {
	Name         string `json:"name"`
	Image        string `json:"image"`
	Ready        bool   `json:"ready"`
	RestartCount int    `json:"restartCount"`
	State        struct {
		Waiting *struct {
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"waiting"`
		Running *struct {
			StartedAt time.Time `json:"startedAt"`
		} `json:"running"`
		Terminated *struct {
			Reason     string    `json:"reason"`
			Message    string    `json:"message"`
			ExitCode   int32     `json:"exitCode"`
			StartedAt  time.Time `json:"startedAt"`
			FinishedAt time.Time `json:"finishedAt"`
		} `json:"terminated"`
	} `json:"state"`
	LastTerminationState struct {
		Terminated *struct {
			Reason     string    `json:"reason"`
			Message    string    `json:"message"`
			ExitCode   int32     `json:"exitCode"`
			StartedAt  time.Time `json:"startedAt"`
			FinishedAt time.Time `json:"finishedAt"`
		} `json:"terminated"`
	} `json:"lastState"`
}

func hydratePods(data []byte) []Pod {
	var l podList
	if err := json.Unmarshal(data, &l); err != nil {
		return nil
	}
	out := make([]Pod, 0, len(l.Items))
	for _, it := range l.Items {
		ready, total, restarts := 0, len(it.Status.ContainerStatuses), 0
		reason := ""
		var exitCode *int32
		oomKilled := false
		containers := hydrateContainers(it.Status.ContainerStatuses)
		initContainers := hydrateContainers(it.Status.InitContainerStatuses)
		for _, cs := range containers {
			if cs.Ready {
				ready++
			}
			restarts += cs.RestartCount
			if reason == "" {
				reason = cs.Reason
			}
			if cs.ExitCode != nil {
				exitCode = cs.ExitCode
			}
			if cs.Reason == "OOMKilled" || cs.LastReason == "OOMKilled" {
				oomKilled = true
			}
		}
		for _, cs := range initContainers {
			if reason == "" {
				reason = cs.Reason
			}
			if cs.ExitCode != nil && exitCode == nil {
				exitCode = cs.ExitCode
			}
			if cs.Reason == "OOMKilled" || cs.LastReason == "OOMKilled" {
				oomKilled = true
			}
		}
		conditions := make([]Condition, 0, len(it.Status.Conditions))
		for _, c := range it.Status.Conditions {
			conditions = append(conditions, Condition{
				Type: c.Type, Status: c.Status, Reason: c.Reason, Message: c.Message,
			})
		}
		claims := podResourceClaims(it.Spec.ResourceClaims, it.Status.ResourceClaimStatuses)
		out = append(out, Pod{
			Name:            it.Metadata.Name,
			UID:             it.Metadata.UID,
			CreatedAt:       it.Metadata.CreationTimestamp,
			Phase:           it.Status.Phase,
			Node:            it.Spec.NodeName,
			Ready:           fmt.Sprintf("%d/%d", ready, total),
			Restarts:        restarts,
			StartedAt:       it.Status.StartTime,
			ResourceClaims:  claims,
			ContainerReason: reason,
			ExitCode:        exitCode,
			OOMKilled:       oomKilled,
			Conditions:      conditions,
			InitContainers:  initContainers,
			Containers:      containers,
		})
	}
	return out
}

func hydrateContainers(items []containerStatusObj) []Container {
	out := make([]Container, 0, len(items))
	for _, cs := range items {
		c := Container{
			Name:         cs.Name,
			Image:        cs.Image,
			Ready:        cs.Ready,
			RestartCount: cs.RestartCount,
		}
		switch {
		case cs.State.Waiting != nil:
			c.State = "waiting"
			c.Reason = cs.State.Waiting.Reason
			c.Message = cs.State.Waiting.Message
		case cs.State.Running != nil:
			c.State = "running"
			c.StartedAt = cs.State.Running.StartedAt
		case cs.State.Terminated != nil:
			c.State = "terminated"
			c.Reason = cs.State.Terminated.Reason
			c.Message = cs.State.Terminated.Message
			c.StartedAt = cs.State.Terminated.StartedAt
			c.FinishedAt = cs.State.Terminated.FinishedAt
			code := cs.State.Terminated.ExitCode
			c.ExitCode = &code
		}
		if cs.LastTerminationState.Terminated != nil {
			c.LastReason = cs.LastTerminationState.Terminated.Reason
			c.LastMessage = cs.LastTerminationState.Terminated.Message
			code := cs.LastTerminationState.Terminated.ExitCode
			c.LastExitCode = &code
		}
		out = append(out, c)
	}
	return out
}

func podResourceClaims(specClaims []struct {
	Name                      string `json:"name"`
	ResourceClaimName         string `json:"resourceClaimName"`
	ResourceClaimTemplateName string `json:"resourceClaimTemplateName"`
}, statusClaims []struct {
	Name              string `json:"name"`
	ResourceClaimName string `json:"resourceClaimName"`
}) []string {
	statusByName := map[string]string{}
	for _, claim := range statusClaims {
		if claim.Name != "" && claim.ResourceClaimName != "" {
			statusByName[claim.Name] = claim.ResourceClaimName
		}
	}
	seen := map[string]bool{}
	var out []string
	for _, claim := range specClaims {
		value := firstNonEmpty(statusByName[claim.Name], claim.ResourceClaimName, claim.ResourceClaimTemplateName, claim.Name)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func mergePods(existing, next []Pod) []Pod {
	seen := map[string]bool{}
	out := make([]Pod, 0, len(existing)+len(next))
	for _, pod := range append(existing, next...) {
		key := firstNonEmpty(pod.UID, pod.Name)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, pod)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func uniquePodResourceClaims(pods []Pod) []string {
	seen := map[string]bool{}
	var names []string
	for _, pod := range pods {
		for _, claim := range pod.ResourceClaims {
			if claim == "" || seen[claim] {
				continue
			}
			seen[claim] = true
			names = append(names, claim)
		}
	}
	sort.Strings(names)
	return names
}
