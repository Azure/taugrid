// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package status

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/kube"
)

type resourceClaimObj struct {
	Metadata struct {
		Name              string    `json:"name"`
		CreationTimestamp time.Time `json:"creationTimestamp"`
	} `json:"metadata"`
	Status struct {
		Allocation json.RawMessage `json:"allocation"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

type resourceClaimList struct {
	Items []resourceClaimObj `json:"items"`
}

func fetchResourceClaims(ctx context.Context, r *kube.Runner, namespace string, names []string) []ResourceClaim {
	if len(names) == 0 {
		return nil
	}
	wanted := stringSet(names)
	raw, err := r.Raw(ctx, []string{"-n", namespace, "get", "resourceclaims", "-o", "json"}, nil)
	if err == nil {
		return hydrateResourceClaimList([]byte(raw), wanted)
	}

	var claims []ResourceClaim
	for _, name := range names {
		raw, err := r.Raw(ctx, []string{"-n", namespace, "get", "resourceclaim", name, "-o", "json"}, nil)
		if err != nil {
			continue
		}
		if claim, ok := hydrateResourceClaim([]byte(raw)); ok {
			claims = append(claims, claim)
		}
	}
	sort.Slice(claims, func(i, j int) bool {
		return claims[i].Name < claims[j].Name
	})
	return claims
}

func hydrateResourceClaimList(data []byte, wanted map[string]bool) []ResourceClaim {
	var list resourceClaimList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil
	}
	claims := make([]ResourceClaim, 0, len(wanted))
	for _, item := range list.Items {
		if !wanted[item.Metadata.Name] {
			continue
		}
		if claim, ok := hydrateResourceClaimObj(item); ok {
			claims = append(claims, claim)
		}
	}
	sort.Slice(claims, func(i, j int) bool {
		return claims[i].Name < claims[j].Name
	})
	return claims
}

func hydrateResourceClaim(data []byte) (ResourceClaim, bool) {
	var obj resourceClaimObj
	if err := json.Unmarshal(data, &obj); err != nil {
		return ResourceClaim{}, false
	}
	return hydrateResourceClaimObj(obj)
}

func hydrateResourceClaimObj(obj resourceClaimObj) (ResourceClaim, bool) {
	allocated, allocation := summarizeAllocation(obj.Status.Allocation)
	claim := ResourceClaim{
		Name:       obj.Metadata.Name,
		CreatedAt:  obj.Metadata.CreationTimestamp,
		Allocated:  allocated,
		Allocation: allocation,
	}
	for _, c := range obj.Status.Conditions {
		condition := Condition{Type: c.Type, Status: c.Status, Reason: c.Reason, Message: c.Message}
		claim.Conditions = append(claim.Conditions, condition)
		if condition.Type == "Allocated" && condition.Status == "True" {
			claim.Allocated = true
		}
		if claim.LastReason == "" && condition.Status != "True" {
			claim.LastReason = condition.Reason
			claim.LastMessage = condition.Message
		}
	}
	return claim, true
}

func summarizeAllocation(raw json.RawMessage) (bool, string) {
	text := strings.TrimSpace(string(raw))
	if text == "" || text == "null" || text == "{}" {
		return false, ""
	}
	var allocation map[string]any
	if err := json.Unmarshal(raw, &allocation); err != nil {
		return true, "allocated"
	}
	var parts []string
	if devices, ok := allocation["devices"].(map[string]any); ok {
		if results, ok := devices["results"].([]any); ok {
			for _, item := range results {
				result, ok := item.(map[string]any)
				if !ok {
					continue
				}
				pool := stringValue(result["pool"])
				device := firstNonEmpty(stringValue(result["device"]), stringValue(result["deviceName"]))
				driver := stringValue(result["driver"])
				value := firstNonEmpty(device, pool, driver)
				if pool != "" && device != "" {
					value = pool + "/" + device
				}
				if value != "" {
					parts = append(parts, value)
				}
			}
		}
	}
	if len(parts) == 0 {
		return true, "allocated"
	}
	sort.Strings(parts)
	return true, strings.Join(parts, ",")
}

func stringValue(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func stringSet(values []string) map[string]bool {
	out := make(map[string]bool, len(values))
	for _, value := range values {
		if value != "" {
			out[value] = true
		}
	}
	return out
}
