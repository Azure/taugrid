// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package status

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/Azure/taugrid/core/kube"
)

type eventList struct {
	Items []struct {
		InvolvedObject struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"involvedObject"`
		Regarding struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"regarding"`
		Type                string    `json:"type"`
		Reason              string    `json:"reason"`
		Message             string    `json:"message"`
		Note                string    `json:"note"`
		Count               int       `json:"count"`
		FirstTimestamp      time.Time `json:"firstTimestamp"`
		LastTimestamp       time.Time `json:"lastTimestamp"`
		EventTime           time.Time `json:"eventTime"`
		DeprecatedFirstTime time.Time `json:"deprecatedFirstTimestamp"`
		DeprecatedLastTime  time.Time `json:"deprecatedLastTimestamp"`
		Series              struct {
			LastObservedTime time.Time `json:"lastObservedTime"`
		} `json:"series"`
	} `json:"items"`
}

func fetchEvents(ctx context.Context, r *kube.Runner, namespace string, names []string) []Event {
	if len(names) == 0 {
		return nil
	}
	raw, err := r.Raw(ctx, []string{"-n", namespace, "get", "events", "-o", "json"}, nil)
	if err != nil {
		return nil
	}
	namesSet := stringSet(names)
	events := filterEvents(hydrateEvents([]byte(raw)), namesSet)
	sort.Slice(events, func(i, j int) bool {
		return events[i].LastSeen.Before(events[j].LastSeen)
	})
	return events
}

func filterEvents(events []Event, names map[string]bool) []Event {
	out := make([]Event, 0, len(events))
	for _, event := range events {
		if names[event.InvolvedName] {
			out = append(out, event)
		}
	}
	return out
}

func hydrateEvents(data []byte) []Event {
	var l eventList
	if err := json.Unmarshal(data, &l); err != nil {
		return nil
	}
	out := make([]Event, 0, len(l.Items))
	for _, item := range l.Items {
		kind := firstNonEmpty(item.InvolvedObject.Kind, item.Regarding.Kind)
		name := firstNonEmpty(item.InvolvedObject.Name, item.Regarding.Name)
		first := firstTime(item.FirstTimestamp, item.DeprecatedFirstTime, item.EventTime, item.Series.LastObservedTime, item.LastTimestamp, item.DeprecatedLastTime)
		last := firstTime(item.LastTimestamp, item.DeprecatedLastTime, item.Series.LastObservedTime, item.EventTime, item.FirstTimestamp, item.DeprecatedFirstTime)
		out = append(out, Event{
			InvolvedKind: kind,
			InvolvedName: name,
			Type:         item.Type,
			Reason:       item.Reason,
			Message:      firstNonEmpty(item.Message, item.Note),
			Count:        item.Count,
			FirstSeen:    first,
			LastSeen:     last,
		})
	}
	return out
}

func eventObjectNames(s Snapshot) []string {
	rj := snapshotRayJob(s)
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" {
			seen[name] = true
		}
	}
	add(s.Name)
	add(rj.RayClusterName)
	for _, w := range s.Workloads {
		add(w.Name)
	}
	for _, p := range s.Pods {
		add(p.Name)
	}
	for _, c := range s.ResourceClaims {
		add(c.Name)
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func firstTime(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}
