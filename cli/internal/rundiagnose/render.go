package rundiagnose

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

func WriteJSON(w io.Writer, snapshot Snapshot) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(snapshot)
}

func WriteText(w io.Writer, snapshot Snapshot) error {
	fmt.Fprintf(w, "Tau run diagnostic: %s/%s\n", snapshot.Run.Namespace, snapshot.Run.Name)
	fmt.Fprintf(w, "Collected: %s\n", snapshot.CollectedAt.Format("2006-01-02T15:04:05Z"))
	fmt.Fprintf(w, "Record source: %s\n", snapshot.Run.RecordSource)
	fmt.Fprintf(w, "State: %s", snapshot.Run.State)
	if len(snapshot.Run.Kinds) > 0 {
		fmt.Fprintf(w, " (%s)", strings.Join(snapshot.Run.Kinds, ", "))
	}
	fmt.Fprintln(w)

	fmt.Fprintln(w, "\nRoot objects:")
	rootCount := len(snapshot.Jobs) + len(snapshot.RayJobs) + len(snapshot.RayClusters)
	if rootCount == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, object := range append(append(append([]WorkloadObject{}, snapshot.Jobs...), snapshot.RayJobs...), snapshot.RayClusters...) {
		writeWorkloadObject(w, object)
	}

	fmt.Fprintln(w, "\nAccess:")
	for _, access := range snapshot.Access {
		fmt.Fprintf(w, "  %-60s %s", access.Resource, access.Status)
		if access.Message != "" {
			fmt.Fprintf(w, ": %s", oneLine(access.Message))
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "\nWorkloads:")
	if len(snapshot.Workloads) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, workload := range snapshot.Workloads {
		fmt.Fprintf(w, "  %s queue=%s", workload.Name, dash(workload.QueueName))
		if clusterQueue, ok := workload.Admission["clusterQueue"]; ok {
			fmt.Fprintf(w, " clusterQueue=%v", clusterQueue)
		}
		if len(workload.Conditions) == 0 {
			fmt.Fprintln(w)
		} else {
			fmt.Fprintln(w)
			for _, condition := range workload.Conditions {
				fmt.Fprintf(w, "    condition %s=%s", condition.Type, condition.Status)
				if condition.Reason != "" {
					fmt.Fprintf(w, " reason=%s", condition.Reason)
				}
				if condition.Message != "" {
					fmt.Fprintf(w, " message=%s", oneLine(condition.Message))
				}
				fmt.Fprintln(w)
			}
		}
		for _, check := range workload.AdmissionChecks {
			fmt.Fprintf(w, "    admissionCheck name=%v state=%v", check["name"], check["state"])
			if message, ok := check["message"]; ok && fmt.Sprint(message) != "" {
				fmt.Fprintf(w, " message=%s", oneLine(fmt.Sprint(message)))
			}
			fmt.Fprintln(w)
		}
	}

	fmt.Fprintln(w, "\nPods:")
	if len(snapshot.Pods) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, pod := range snapshot.Pods {
		fmt.Fprintf(w, "  %s phase=%s node=%s\n", pod.Name, dash(pod.Phase), dash(pod.NodeName))
		containers := append(append([]ContainerState{}, pod.InitContainers...), pod.Containers...)
		for _, container := range containers {
			fmt.Fprintf(w, "    %s state=%s restarts=%d", container.Name, dash(container.Current.State), container.RestartCount)
			if container.Current.Reason != "" {
				fmt.Fprintf(w, " reason=%s", container.Current.Reason)
			}
			if container.Current.ExitCode != nil {
				fmt.Fprintf(w, " exit=%d", *container.Current.ExitCode)
			}
			if container.Last.State != "" {
				fmt.Fprintf(w, " last=%s", container.Last.State)
				if container.Last.Reason != "" {
					fmt.Fprintf(w, "/%s", container.Last.Reason)
				}
				if container.Last.ExitCode != nil {
					fmt.Fprintf(w, " exit=%d", *container.Last.ExitCode)
				}
			}
			fmt.Fprintln(w)
		}
	}

	fmt.Fprintln(w, "\nEvents:")
	if len(snapshot.Events) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, event := range snapshot.Events {
		fmt.Fprintf(w, "  %s %s/%s %s: %s\n", dash(event.Type), dash(event.Regarding.Kind), event.Regarding.Name, dash(event.Reason), oneLine(event.Message))
	}

	fmt.Fprintln(w, "\nLogs:")
	if len(snapshot.Logs) == 0 {
		fmt.Fprintln(w, "  (none)")
	}
	for _, log := range snapshot.Logs {
		previous := ""
		if log.Previous {
			previous = " previous"
		}
		role := ""
		if log.Role != "" {
			role = " role=" + log.Role
		}
		fmt.Fprintf(w, "--- %s/%s%s [%s%s] ---\n", log.Pod, log.Container, previous, log.Status, role)
		if log.Message != "" {
			fmt.Fprintln(w, log.Message)
		}
		if log.Text != "" {
			fmt.Fprint(w, log.Text)
			if !strings.HasSuffix(log.Text, "\n") {
				fmt.Fprintln(w)
			}
		}
	}

	if len(snapshot.Warnings) > 0 {
		fmt.Fprintln(w, "\nWarnings:")
		for _, warning := range snapshot.Warnings {
			fmt.Fprintf(w, "  - %s\n", warning)
		}
	}
	return nil
}

func writeWorkloadObject(w io.Writer, object WorkloadObject) {
	fmt.Fprintf(w, "  %s %s uid=%s\n", dash(object.Kind), object.Name, dash(object.UID))
	keys := make([]string, 0, len(object.Status))
	for key := range object.Status {
		if key != "conditions" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(w, "    %s=%s\n", key, compactValue(object.Status[key]))
	}
	for _, condition := range mapConditions(object.Status["conditions"]) {
		fmt.Fprintf(w, "    condition %s=%s", condition.Type, condition.Status)
		if condition.Reason != "" {
			fmt.Fprintf(w, " reason=%s", condition.Reason)
		}
		if condition.Message != "" {
			fmt.Fprintf(w, " message=%s", oneLine(condition.Message))
		}
		fmt.Fprintln(w)
	}
}

func mapConditions(value any) []Condition {
	raw, _ := value.([]any)
	out := make([]Condition, 0, len(raw))
	for _, item := range raw {
		fields, _ := item.(map[string]any)
		out = append(out, Condition{
			Type:    stringField(fields, "type"),
			Status:  stringField(fields, "status"),
			Reason:  stringField(fields, "reason"),
			Message: stringField(fields, "message"),
		})
	}
	return out
}

func stringField(fields map[string]any, key string) string {
	if value, ok := fields[key]; ok && value != nil {
		return fmt.Sprint(value)
	}
	return ""
}

func compactValue(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return oneLine(fmt.Sprint(value))
	}
	return string(data)
}

func oneLine(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

func dash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
