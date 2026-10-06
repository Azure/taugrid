// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

// Package quota builds a read-only workspace and team quota snapshot from
// Kueue v1beta2 objects.
package quota

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
)

const dnsLabelMaxLength = 63

// Reader fetches only the exact Kueue objects selected by a resolved workspace.
type Reader interface {
	GetLocalQueue(ctx context.Context, namespace, name string) ([]byte, error)
	GetClusterQueue(ctx context.Context, name string) ([]byte, error)
	GetCohort(ctx context.Context, name string) ([]byte, error)
}

// Scope is the authorization-resolved workspace identity used for exact reads.
type Scope struct {
	Workspace  string
	Team       string
	Namespace  string
	LocalQueue string
}

// Snapshot reports workspace quota and, when configured, team shared quota.
type Snapshot struct {
	Workspace       QueueQuota `json:"workspace"`
	Team            *TeamQuota `json:"team,omitempty"`
	Legacy          bool       `json:"legacy"`
	TeamUnavailable string     `json:"teamUnavailable,omitempty"`
}

// QueueQuota is the configured and observed quota for one ClusterQueue.
type QueueQuota struct {
	Name                 string          `json:"name"`
	Cohort               string          `json:"cohort,omitempty"`
	Resources            []ResourceQuota `json:"resources"`
	PendingWorkloads     int64           `json:"pendingWorkloads"`
	ReservingWorkloads   int64           `json:"reservingWorkloads"`
	AdmittedWorkloads    int64           `json:"admittedWorkloads"`
	UsageAvailable       bool            `json:"usageAvailable"`
	ReservationAvailable bool            `json:"reservationAvailable"`
}

// TeamQuota is the shared pool configured on a team Cohort. Kueue v1beta2
// exposes fair-sharing status but not per-resource Cohort usage.
type TeamQuota struct {
	Name             string          `json:"name"`
	Resources        []ResourceQuota `json:"resources"`
	WeightedShare    string          `json:"weightedShare,omitempty"`
	UsageAvailable   bool            `json:"usageAvailable"`
	UsageUnavailable string          `json:"usageUnavailable,omitempty"`
}

// ResourceQuota reports one flavor/resource tuple using Kubernetes quantity
// strings so CPU, memory, GPU, and extended resources retain exact units.
type ResourceQuota struct {
	Flavor         string `json:"flavor"`
	Resource       string `json:"resource"`
	Nominal        string `json:"nominal"`
	BorrowingLimit string `json:"borrowingLimit,omitempty"`
	LendingLimit   string `json:"lendingLimit,omitempty"`
	Reserved       string `json:"reserved,omitempty"`
	Used           string `json:"used,omitempty"`
	Borrowed       string `json:"borrowed,omitempty"`
}

type kueueObject struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		ClusterQueue   string          `json:"clusterQueue"`
		CohortName     string          `json:"cohortName"`
		ResourceGroups []resourceGroup `json:"resourceGroups"`
	} `json:"spec"`
	Status struct {
		PendingWorkloads   int64         `json:"pendingWorkloads"`
		ReservingWorkloads int64         `json:"reservingWorkloads"`
		AdmittedWorkloads  int64         `json:"admittedWorkloads"`
		FlavorsReservation []flavorUsage `json:"flavorsReservation"`
		FlavorsUsage       []flavorUsage `json:"flavorsUsage"`
		FairSharing        struct {
			WeightedShare json.RawMessage `json:"weightedShare"`
		} `json:"fairSharing"`
	} `json:"status"`
}

type resourceGroup struct {
	Flavors []struct {
		Name      string `json:"name"`
		Resources []struct {
			Name           string          `json:"name"`
			NominalQuota   json.RawMessage `json:"nominalQuota"`
			BorrowingLimit json.RawMessage `json:"borrowingLimit"`
			LendingLimit   json.RawMessage `json:"lendingLimit"`
		} `json:"resources"`
	} `json:"flavors"`
}

type flavorUsage struct {
	Name      string `json:"name"`
	Resources []struct {
		Name     string          `json:"name"`
		Total    json.RawMessage `json:"total"`
		Borrowed json.RawMessage `json:"borrowed"`
	} `json:"resources"`
}

// Read fetches the exact workspace ClusterQueue and optional team Cohort.
func Read(ctx context.Context, reader Reader, scope Scope) (Snapshot, error) {
	if reader == nil {
		return Snapshot{}, errors.New("portal started without Kubernetes quota access")
	}
	queueName := managedName("tau-ws", scope.Workspace)
	queueRaw, err := reader.GetClusterQueue(ctx, queueName)
	if apierrors.IsNotFound(err) && scope.Namespace != "" && scope.LocalQueue != "" {
		localRaw, localErr := reader.GetLocalQueue(ctx, scope.Namespace, scope.LocalQueue)
		if localErr != nil {
			return Snapshot{}, fmt.Errorf("read workspace LocalQueue: %w", localErr)
		}
		var local kueueObject
		if err := json.Unmarshal(localRaw, &local); err != nil {
			return Snapshot{}, fmt.Errorf("decode workspace LocalQueue: %w", err)
		}
		if strings.TrimSpace(local.Spec.ClusterQueue) == "" {
			return Snapshot{}, errors.New("workspace LocalQueue does not reference a ClusterQueue")
		}
		queueName = local.Spec.ClusterQueue
		queueRaw, err = reader.GetClusterQueue(ctx, queueName)
	}
	if err != nil {
		return Snapshot{}, fmt.Errorf("read workspace ClusterQueue %q: %w", queueName, err)
	}
	queue, err := decodeQueue(queueRaw)
	if err != nil {
		return Snapshot{}, fmt.Errorf("decode workspace ClusterQueue %q: %w", queueName, err)
	}
	snapshot := Snapshot{Workspace: queue}

	team := strings.TrimSpace(scope.Team)
	cohortName := strings.TrimSpace(queue.Cohort)
	if team == "" || cohortName == "" {
		snapshot.Legacy = true
		return snapshot, nil
	}
	expectedCohort := managedName("tau-team", team)
	if cohortName != expectedCohort {
		snapshot.Legacy = true
		return snapshot, nil
	}
	cohortRaw, err := reader.GetCohort(ctx, cohortName)
	if apierrors.IsNotFound(err) {
		snapshot.Legacy = true
		return snapshot, nil
	}
	if err != nil {
		snapshot.TeamUnavailable = fmt.Sprintf("team Cohort is unavailable: %v", err)
		return snapshot, nil
	}
	cohort, err := decodeTeam(cohortRaw)
	if err != nil {
		snapshot.TeamUnavailable = fmt.Sprintf("team Cohort status is unreadable: %v", err)
		return snapshot, nil
	}
	snapshot.Team = &cohort
	return snapshot, nil
}

func decodeQueue(raw []byte) (QueueQuota, error) {
	var object kueueObject
	if err := json.Unmarshal(raw, &object); err != nil {
		return QueueQuota{}, err
	}
	resources, err := quotaResources(object.Spec.ResourceGroups)
	if err != nil {
		return QueueQuota{}, err
	}
	reserved := usageValues(object.Status.FlavorsReservation)
	used := usageValues(object.Status.FlavorsUsage)
	for i := range resources {
		key := resources[i].Flavor + "\x00" + resources[i].Resource
		if value, ok := reserved[key]; ok {
			resources[i].Reserved = value.total
		}
		if value, ok := used[key]; ok {
			resources[i].Used = value.total
			resources[i].Borrowed = value.borrowed
		}
	}
	return QueueQuota{
		Name: object.Metadata.Name, Cohort: object.Spec.CohortName, Resources: resources,
		PendingWorkloads:     object.Status.PendingWorkloads,
		ReservingWorkloads:   object.Status.ReservingWorkloads,
		AdmittedWorkloads:    object.Status.AdmittedWorkloads,
		UsageAvailable:       object.Status.FlavorsUsage != nil,
		ReservationAvailable: object.Status.FlavorsReservation != nil,
	}, nil
}

func decodeTeam(raw []byte) (TeamQuota, error) {
	var object kueueObject
	if err := json.Unmarshal(raw, &object); err != nil {
		return TeamQuota{}, err
	}
	resources, err := quotaResources(object.Spec.ResourceGroups)
	if err != nil {
		return TeamQuota{}, err
	}
	return TeamQuota{
		Name: object.Metadata.Name, Resources: resources,
		WeightedShare:    quantityString(object.Status.FairSharing.WeightedShare),
		UsageAvailable:   false,
		UsageUnavailable: "Kueue v1beta2 Cohort status does not report per-resource usage",
	}, nil
}

func quotaResources(groups []resourceGroup) ([]ResourceQuota, error) {
	var out []ResourceQuota
	for _, group := range groups {
		for _, flavor := range group.Flavors {
			for _, item := range flavor.Resources {
				nominal := quantityString(item.NominalQuota)
				if nominal == "" {
					return nil, fmt.Errorf("flavor %q resource %q has no nominalQuota", flavor.Name, item.Name)
				}
				out = append(out, ResourceQuota{
					Flavor: flavor.Name, Resource: item.Name, Nominal: nominal,
					BorrowingLimit: quantityString(item.BorrowingLimit),
					LendingLimit:   quantityString(item.LendingLimit),
				})
			}
		}
	}
	return out, nil
}

type usageValue struct {
	total    string
	borrowed string
}

func usageValues(flavors []flavorUsage) map[string]usageValue {
	out := make(map[string]usageValue)
	for _, flavor := range flavors {
		for _, item := range flavor.Resources {
			out[flavor.Name+"\x00"+item.Name] = usageValue{
				total: quantityString(item.Total), borrowed: quantityString(item.Borrowed),
			}
		}
	}
	return out
}

func quantityString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var value string
	if raw[0] == '"' {
		if json.Unmarshal(raw, &value) != nil {
			return ""
		}
	} else {
		value = string(raw)
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return ""
	}
	return quantity.String()
}

func managedName(prefix, identity string) string {
	value := prefix + "-" + identity
	if len(value) <= dnsLabelMaxLength {
		return value
	}
	sum := sha256.Sum256([]byte(value))
	suffix := hex.EncodeToString(sum[:6])
	baseLength := dnsLabelMaxLength - len(suffix) - 1
	base := strings.TrimRight(value[:baseLength], "-_.")
	return base + "-" + suffix
}
