// Package rundiagnose gathers a bounded, read-only snapshot of one Tau run.
package rundiagnose

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Azure/taugrid/cli/internal/raylogoffload"
	"github.com/Azure/taugrid/core/workloadmeta"
)

const (
	APIVersion           = "diagnostics.tau.azure.com/v1alpha1"
	Kind                 = "RunDiagnostic"
	defaultMaxLogStreams = 24
	DefaultTailLines     = 100
	DefaultLogLimitBytes = 65536
	DefaultEventLimit    = 50
	MaxTailLines         = 1000
	MaxLogLimitBytes     = 1 << 20
	MaxEventLimit        = 500
	MaxWorkloads         = 32
	MaxPods              = 128
	MaxEventSubjects     = 64
	MaxStatusItems       = 64
	MaxContainersPerPod  = 32
	MaxFieldBytes        = 8192
)

type Runner interface {
	Raw(context.Context, []string, []byte) (string, error)
}

type Options struct {
	Namespace     string
	Context       string
	TailLines     int
	LogLimitBytes int
	EventLimit    int
	Now           func() time.Time
}

type Limits struct {
	TailLines        int `json:"tailLines"`
	LogLimitBytes    int `json:"logLimitBytes"`
	EventLimit       int `json:"eventLimit"`
	MaxLogStreams    int `json:"maxLogStreams"`
	MaxWorkloads     int `json:"maxWorkloads"`
	MaxPods          int `json:"maxPods"`
	MaxEventSubjects int `json:"maxEventSubjects"`
}

type Snapshot struct {
	APIVersion  string           `json:"apiVersion"`
	Kind        string           `json:"kind"`
	CollectedAt time.Time        `json:"collectedAt"`
	Run         Run              `json:"run"`
	Limits      Limits           `json:"limits"`
	Access      []Access         `json:"access"`
	Jobs        []WorkloadObject `json:"jobs"`
	RayJobs     []WorkloadObject `json:"rayJobs"`
	RayClusters []WorkloadObject `json:"rayClusters"`
	Workloads   []KueueWorkload  `json:"workloads"`
	Pods        []Pod            `json:"pods"`
	Events      []Event          `json:"events"`
	Logs        []ContainerLog   `json:"logs"`
	Warnings    []string         `json:"warnings"`
}

type Run struct {
	Name         string            `json:"name"`
	Namespace    string            `json:"namespace"`
	Context      string            `json:"context,omitempty"`
	RecordSource string            `json:"recordSource"`
	State        string            `json:"state"`
	Kinds        []string          `json:"kinds"`
	UIDs         []string          `json:"uids"`
	Labels       map[string]string `json:"labels,omitempty"`
	Annotations  map[string]string `json:"annotations,omitempty"`
}

type Access struct {
	Resource string `json:"resource"`
	Status   string `json:"status"`
	Message  string `json:"message,omitempty"`
}

type ObjectRef struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Name       string `json:"name"`
	UID        string `json:"uid,omitempty"`
}

type WorkloadObject struct {
	ObjectRef
	Namespace   string            `json:"namespace"`
	CreatedAt   time.Time         `json:"createdAt,omitzero"`
	Generation  int64             `json:"generation,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
	Owners      []ObjectRef       `json:"owners"`
	Spec        map[string]any    `json:"spec,omitempty"`
	Status      map[string]any    `json:"status,omitempty"`
}

type Condition struct {
	Type               string    `json:"type"`
	Status             string    `json:"status"`
	Reason             string    `json:"reason,omitempty"`
	Message            string    `json:"message,omitempty"`
	LastTransitionTime time.Time `json:"lastTransitionTime,omitzero"`
}

type KueueWorkload struct {
	ObjectRef
	Namespace         string           `json:"namespace"`
	CreatedAt         time.Time        `json:"createdAt,omitzero"`
	Owners            []ObjectRef      `json:"owners"`
	QueueName         string           `json:"queueName,omitempty"`
	PriorityClassName string           `json:"priorityClassName,omitempty"`
	Admission         map[string]any   `json:"admission,omitempty"`
	AdmissionChecks   []map[string]any `json:"admissionChecks"`
	Conditions        []Condition      `json:"conditions"`
}

type ContainerState struct {
	Name         string      `json:"name"`
	Image        string      `json:"image,omitempty"`
	Ready        bool        `json:"ready"`
	RestartCount int         `json:"restartCount"`
	Current      StateDetail `json:"current,omitzero"`
	Last         StateDetail `json:"last,omitzero"`
}

type StateDetail struct {
	State      string    `json:"state,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Message    string    `json:"message,omitempty"`
	ExitCode   *int32    `json:"exitCode,omitempty"`
	Signal     *int32    `json:"signal,omitempty"`
	StartedAt  time.Time `json:"startedAt,omitzero"`
	FinishedAt time.Time `json:"finishedAt,omitzero"`
}

type Pod struct {
	ObjectRef
	Namespace      string           `json:"namespace"`
	CreatedAt      time.Time        `json:"createdAt,omitzero"`
	Owners         []ObjectRef      `json:"owners"`
	Phase          string           `json:"phase,omitempty"`
	NodeName       string           `json:"nodeName,omitempty"`
	Conditions     []Condition      `json:"conditions"`
	InitContainers []ContainerState `json:"initContainers"`
	Containers     []ContainerState `json:"containers"`
}

type Event struct {
	Regarding ObjectRef `json:"regarding"`
	Type      string    `json:"type,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	Message   string    `json:"message,omitempty"`
	Count     int       `json:"count,omitempty"`
	FirstSeen time.Time `json:"firstSeen,omitzero"`
	LastSeen  time.Time `json:"lastSeen,omitzero"`
}

type ContainerLog struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	Role      string `json:"role,omitempty"`
	Previous  bool   `json:"previous,omitempty"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
	Text      string `json:"text,omitempty"`
}

type metadata struct {
	Name              string            `json:"name"`
	Namespace         string            `json:"namespace"`
	UID               string            `json:"uid"`
	Generation        int64             `json:"generation"`
	CreationTimestamp time.Time         `json:"creationTimestamp"`
	Labels            map[string]string `json:"labels"`
	Annotations       map[string]string `json:"annotations"`
	OwnerReferences   []struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Name       string `json:"name"`
		UID        string `json:"uid"`
	} `json:"ownerReferences"`
}

type genericObject struct {
	APIVersion string         `json:"apiVersion"`
	Kind       string         `json:"kind"`
	Metadata   metadata       `json:"metadata"`
	Spec       map[string]any `json:"spec"`
	Status     map[string]any `json:"status"`
}

type genericList struct {
	Items []json.RawMessage `json:"items"`
}

func Gather(ctx context.Context, runner Runner, name string, opts Options) (Snapshot, error) {
	if runner == nil {
		return Snapshot{}, fmt.Errorf("diagnostic runner is required")
	}
	if strings.TrimSpace(name) == "" {
		return Snapshot{}, fmt.Errorf("run name is required")
	}
	if strings.TrimSpace(opts.Namespace) == "" {
		return Snapshot{}, fmt.Errorf("namespace is required")
	}
	if opts.TailLines < 0 {
		return Snapshot{}, fmt.Errorf("tail lines must be >= 0")
	}
	if opts.TailLines > MaxTailLines {
		return Snapshot{}, fmt.Errorf("tail lines must be <= %d", MaxTailLines)
	}
	if opts.LogLimitBytes <= 0 {
		return Snapshot{}, fmt.Errorf("log limit bytes must be > 0")
	}
	if opts.LogLimitBytes > MaxLogLimitBytes {
		return Snapshot{}, fmt.Errorf("log limit bytes must be <= %d", MaxLogLimitBytes)
	}
	if opts.EventLimit < 0 {
		return Snapshot{}, fmt.Errorf("event limit must be >= 0")
	}
	if opts.EventLimit > MaxEventLimit {
		return Snapshot{}, fmt.Errorf("event limit must be <= %d", MaxEventLimit)
	}

	now := time.Now().UTC()
	if opts.Now != nil {
		now = opts.Now().UTC()
	}
	s := Snapshot{
		APIVersion:  APIVersion,
		Kind:        Kind,
		CollectedAt: now,
		Run: Run{
			Name:         name,
			Namespace:    opts.Namespace,
			Context:      opts.Context,
			RecordSource: "live-kubernetes",
			State:        "absent",
			Kinds:        []string{},
			UIDs:         []string{},
		},
		Limits: Limits{
			TailLines:        opts.TailLines,
			LogLimitBytes:    opts.LogLimitBytes,
			EventLimit:       opts.EventLimit,
			MaxLogStreams:    defaultMaxLogStreams,
			MaxWorkloads:     MaxWorkloads,
			MaxPods:          MaxPods,
			MaxEventSubjects: MaxEventSubjects,
		},
		Access:      []Access{},
		Jobs:        []WorkloadObject{},
		RayJobs:     []WorkloadObject{},
		RayClusters: []WorkloadObject{},
		Workloads:   []KueueWorkload{},
		Pods:        []Pod{},
		Events:      []Event{},
		Logs:        []ContainerLog{},
		Warnings:    []string{},
	}

	job, jobOK, jobAccess := fetchRunObject(ctx, runner, &s, opts.Namespace, "job", "Job", name, false)
	rayJob, rayOK, rayAccess := fetchRunObject(ctx, runner, &s, opts.Namespace, "rayjob.ray.io", "RayJob", name, true)
	if jobOK {
		s.Jobs = append(s.Jobs, summarizeWorkloadObject(job, "Job"))
		addRunIdentity(&s, job)
	}
	if rayOK {
		s.RayJobs = append(s.RayJobs, summarizeWorkloadObject(rayJob, "RayJob"))
		addRunIdentity(&s, rayJob)
	}
	switch len(s.Run.Kinds) {
	case 0:
		if conclusiveAbsence(jobAccess) && conclusiveAbsence(rayAccess) {
			s.Run.State = "absent"
		} else {
			s.Run.State = "unknown"
		}
	case 1:
		s.Run.State = "found"
	default:
		s.Run.State = "ambiguous"
		s.Warnings = append(s.Warnings, "both a Tau Job and RayJob exist with this run name; one may be a stale object")
	}

	rootUIDs := map[string]bool{}
	for _, uid := range s.Run.UIDs {
		rootUIDs[uid] = true
	}
	rayClusterName := ""
	if rayOK {
		rayClusterName = nestedString(rayJob.Status, "rayClusterName")
	}
	podOwnerUIDs := cloneSet(rootUIDs)
	if rayOK && rayClusterName != "" {
		rayCluster, rayClusterOK := fetchOwnedObject(
			ctx,
			runner,
			&s,
			opts.Namespace,
			"raycluster.ray.io",
			"RayCluster",
			rayClusterName,
			rayJob.Metadata.UID,
			true,
		)
		if rayClusterOK {
			s.RayClusters = append(s.RayClusters, summarizeWorkloadObject(rayCluster, "RayCluster"))
			podOwnerUIDs[rayCluster.Metadata.UID] = true
		}
	} else if rayOK {
		s.Access = append(s.Access, Access{
			Resource: "raycluster.ray.io/<pending>",
			Status:   "absent",
			Message:  "RayJob has not reported status.rayClusterName; no owned RayCluster exists yet",
		})
	}

	workloads := fetchWorkloads(ctx, runner, &s, opts.Namespace, name, s.Run.UIDs)
	validWorkloads := 0
	for _, raw := range workloads {
		var obj genericObject
		if json.Unmarshal(raw, &obj) != nil {
			continue
		}
		if !ownsRun(obj.Metadata, name, rootUIDs) {
			s.Warnings = append(s.Warnings, staleDescendantWarning(obj, rootUIDs))
			continue
		}
		validWorkloads++
		if len(s.Workloads) < MaxWorkloads {
			s.Workloads = append(s.Workloads, summarizeWorkload(obj))
		}
	}
	if validWorkloads > MaxWorkloads {
		s.Warnings = append(s.Warnings, fmt.Sprintf("Kueue Workloads were truncated from %d to %d", validWorkloads, MaxWorkloads))
	}
	sort.Slice(s.Workloads, func(i, j int) bool { return s.Workloads[i].Name < s.Workloads[j].Name })

	pods := fetchPods(ctx, runner, &s, opts.Namespace, name, rayClusterName)
	validPods := 0
	for _, raw := range pods {
		var obj genericObject
		if json.Unmarshal(raw, &obj) != nil {
			continue
		}
		if len(podOwnerUIDs) == 0 {
			s.Warnings = append(s.Warnings, "matching pods were excluded because no current Job/RayJob UID was available to prove ownership")
			continue
		}
		if !ownsRun(obj.Metadata, name, podOwnerUIDs) {
			s.Warnings = append(s.Warnings, staleDescendantWarning(obj, podOwnerUIDs))
			continue
		}
		validPods++
		if len(s.Pods) < MaxPods {
			s.Pods = append(s.Pods, summarizePod(obj))
		}
	}
	if validPods > MaxPods {
		s.Warnings = append(s.Warnings, fmt.Sprintf("Pods were truncated from %d to %d", validPods, MaxPods))
	}
	sort.Slice(s.Pods, func(i, j int) bool { return s.Pods[i].Name < s.Pods[j].Name })
	if s.Run.State == "absent" && (len(s.Workloads) > 0 || len(s.Pods) > 0) {
		s.Run.State = "incomplete"
		s.Warnings = append(s.Warnings, "the root Job/RayJob is absent, but owned descendants remain; creation or cleanup may be incomplete")
	} else if s.Run.State == "unknown" && (len(s.Workloads) > 0 || len(s.Pods) > 0) {
		s.Run.State = "partial"
		s.Warnings = append(s.Warnings, "owned descendants were found, but access to the root Job/RayJob was incomplete")
	}

	uidRefs := make([]ObjectRef, 0, len(s.Jobs)+len(s.RayJobs)+len(s.RayClusters)+len(s.Workloads)+len(s.Pods))
	for _, obj := range s.Jobs {
		uidRefs = append(uidRefs, obj.ObjectRef)
	}
	for _, obj := range s.RayJobs {
		uidRefs = append(uidRefs, obj.ObjectRef)
	}
	for _, obj := range s.RayClusters {
		uidRefs = append(uidRefs, obj.ObjectRef)
	}
	for _, obj := range s.Workloads {
		uidRefs = append(uidRefs, obj.ObjectRef)
	}
	for _, obj := range s.Pods {
		uidRefs = append(uidRefs, obj.ObjectRef)
	}
	s.Events = fetchEvents(ctx, runner, &s, opts.Namespace, uidRefs, opts.EventLimit)
	s.Logs = fetchLogs(ctx, runner, &s, opts.Namespace, s.Pods, opts)
	s.Warnings = uniqueSorted(s.Warnings)
	return s, nil
}

func fetchRunObject(ctx context.Context, runner Runner, s *Snapshot, namespace, resource, kind, name string, customResource bool) (genericObject, bool, string) {
	args := []string{"-n", namespace, "get", resource, name, "-o", "json"}
	out, err := runner.Raw(ctx, args, nil)
	source := resource + "/" + name
	if err != nil {
		status, message := classifyError(err, name, customResource)
		s.Access = append(s.Access, Access{Resource: source, Status: status, Message: message})
		return genericObject{}, false, status
	}
	var obj genericObject
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		s.Access = append(s.Access, Access{Resource: source, Status: "error", Message: "decode response: " + redactField(err.Error())})
		return genericObject{}, false, "error"
	}
	if !tauOwnedRoot(obj.Metadata, name) {
		s.Access = append(s.Access, Access{
			Resource: source,
			Status:   "excluded",
			Message:  "object exists but is not owned by Tau for this run",
		})
		s.Warnings = append(s.Warnings, fmt.Sprintf("%s %s/%s was excluded because its Tau ownership labels do not match", kind, namespace, name))
		return genericObject{}, false, "excluded"
	}
	if obj.Kind == "" {
		obj.Kind = kind
	}
	s.Access = append(s.Access, Access{Resource: source, Status: "available"})
	return obj, true, "available"
}

func fetchOwnedObject(
	ctx context.Context,
	runner Runner,
	s *Snapshot,
	namespace, resource, kind, name, ownerUID string,
	customResource bool,
) (genericObject, bool) {
	source := resource + "/" + name
	out, err := runner.Raw(ctx, []string{"-n", namespace, "get", resource, name, "-o", "json"}, nil)
	if err != nil {
		status, message := classifyError(err, name, customResource)
		s.Access = append(s.Access, Access{Resource: source, Status: status, Message: message})
		return genericObject{}, false
	}
	var obj genericObject
	if err := json.Unmarshal([]byte(out), &obj); err != nil {
		s.Access = append(s.Access, Access{Resource: source, Status: "error", Message: "decode response: " + redactField(err.Error())})
		return genericObject{}, false
	}
	if !ownedByUID(obj.Metadata, ownerUID) {
		s.Access = append(s.Access, Access{
			Resource: source,
			Status:   "excluded",
			Message:  "object exists but its owner UID does not match the current run",
		})
		s.Warnings = append(s.Warnings, fmt.Sprintf("%s %s/%s was excluded because its owner UID does not match the current run", kind, namespace, name))
		return genericObject{}, false
	}
	if obj.Kind == "" {
		obj.Kind = kind
	}
	s.Access = append(s.Access, Access{Resource: source, Status: "available"})
	return obj, true
}

func conclusiveAbsence(status string) bool {
	return status == "absent" || status == "unsupported" || status == "excluded"
}

func fetchWorkloads(ctx context.Context, runner Runner, s *Snapshot, namespace, name string, rootUIDs []string) []json.RawMessage {
	selectors := []string{workloadmeta.LabelJob + "=" + name}
	for _, uid := range rootUIDs {
		if uid != "" {
			selectors = append(selectors, "kueue.x-k8s.io/job-uid="+uid)
		}
	}
	return fetchLists(ctx, runner, s, namespace, "workloads.kueue.x-k8s.io", selectors, true)
}

func fetchPods(ctx context.Context, runner Runner, s *Snapshot, namespace, name, rayClusterName string) []json.RawMessage {
	selectors := []string{"job-name=" + name, workloadmeta.LabelJob + "=" + name}
	if rayClusterName != "" {
		selectors = append(selectors, "ray.io/cluster="+rayClusterName)
	}
	return fetchLists(ctx, runner, s, namespace, "pods", selectors, false)
}

func fetchLists(ctx context.Context, runner Runner, s *Snapshot, namespace, resource string, selectors []string, customResource bool) []json.RawMessage {
	seenSelectors := map[string]bool{}
	seenUIDs := map[string]bool{}
	var result []json.RawMessage
	for _, selector := range selectors {
		if selector == "" || seenSelectors[selector] {
			continue
		}
		seenSelectors[selector] = true
		source := resource + "?selector=" + selector
		out, err := runner.Raw(ctx, []string{"-n", namespace, "get", resource, "-l", selector, "-o", "json"}, nil)
		if err != nil {
			status, message := classifyError(err, "", customResource)
			s.Access = append(s.Access, Access{Resource: source, Status: status, Message: message})
			continue
		}
		var list genericList
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			s.Access = append(s.Access, Access{Resource: source, Status: "error", Message: "decode response: " + redactField(err.Error())})
			continue
		}
		status := "available"
		if len(list.Items) == 0 {
			status = "absent"
		}
		s.Access = append(s.Access, Access{Resource: source, Status: status})
		for _, raw := range list.Items {
			var header struct {
				Metadata metadata `json:"metadata"`
			}
			if json.Unmarshal(raw, &header) != nil {
				continue
			}
			key := header.Metadata.UID
			if key == "" {
				key = header.Metadata.Namespace + "/" + header.Metadata.Name
			}
			if seenUIDs[key] {
				continue
			}
			seenUIDs[key] = true
			result = append(result, raw)
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		return objectSortKey(result[i]) < objectSortKey(result[j])
	})
	return result
}

func fetchEvents(ctx context.Context, runner Runner, s *Snapshot, namespace string, refs []ObjectRef, limit int) []Event {
	if limit == 0 {
		return []Event{}
	}
	var events []Event
	seenUIDs := map[string]bool{}
	orderedRefs := make([]ObjectRef, 0, len(refs))
	for _, ref := range refs {
		if ref.UID == "" || seenUIDs[ref.UID] {
			continue
		}
		seenUIDs[ref.UID] = true
		orderedRefs = append(orderedRefs, ref)
	}
	if len(orderedRefs) > MaxEventSubjects {
		s.Warnings = append(s.Warnings, fmt.Sprintf(
			"event collection subjects were truncated from %d to %d",
			len(orderedRefs),
			MaxEventSubjects,
		))
		orderedRefs = orderedRefs[:MaxEventSubjects]
	}
	for _, ref := range orderedRefs {
		uid := ref.UID
		source := "events?involvedObject.uid=" + uid
		out, err := runner.Raw(ctx, []string{
			"-n", namespace, "get", "events",
			"--field-selector", "involvedObject.uid=" + uid,
			"--sort-by=.lastTimestamp", "-o", "json",
		}, nil)
		if err != nil {
			status, message := classifyError(err, "", false)
			s.Access = append(s.Access, Access{Resource: source, Status: status, Message: message})
			continue
		}
		var list struct {
			Items []struct {
				InvolvedObject ObjectRef `json:"involvedObject"`
				Regarding      ObjectRef `json:"regarding"`
				Type           string    `json:"type"`
				Reason         string    `json:"reason"`
				Message        string    `json:"message"`
				Count          int       `json:"count"`
				FirstTimestamp time.Time `json:"firstTimestamp"`
				LastTimestamp  time.Time `json:"lastTimestamp"`
				EventTime      time.Time `json:"eventTime"`
				Series         struct {
					Count            int       `json:"count"`
					LastObservedTime time.Time `json:"lastObservedTime"`
				} `json:"series"`
			} `json:"items"`
		}
		if err := json.Unmarshal([]byte(out), &list); err != nil {
			s.Access = append(s.Access, Access{Resource: source, Status: "error", Message: "decode response: " + redactField(err.Error())})
			continue
		}
		status := "available"
		if len(list.Items) == 0 {
			status = "absent"
		}
		s.Access = append(s.Access, Access{Resource: source, Status: status})
		for _, item := range list.Items {
			regarding := item.Regarding
			if regarding.Name == "" {
				regarding = item.InvolvedObject
			}
			if regarding.UID == "" {
				regarding = ref
			}
			count := item.Count
			if item.Series.Count > count {
				count = item.Series.Count
			}
			last := item.LastTimestamp
			if item.Series.LastObservedTime.After(last) {
				last = item.Series.LastObservedTime
			}
			if item.EventTime.After(last) {
				last = item.EventTime
			}
			events = append(events, Event{
				Regarding: regarding,
				Type:      item.Type,
				Reason:    redactField(item.Reason),
				Message:   redactField(item.Message),
				Count:     count,
				FirstSeen: item.FirstTimestamp,
				LastSeen:  last,
			})
		}
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].LastSeen.Before(events[j].LastSeen) })
	if len(events) > limit {
		s.Warnings = append(s.Warnings, fmt.Sprintf("events were truncated from %d to the newest %d", len(events), limit))
		events = events[len(events)-limit:]
	}
	return events
}

func fetchLogs(ctx context.Context, runner Runner, s *Snapshot, namespace string, pods []Pod, opts Options) []ContainerLog {
	if opts.TailLines == 0 {
		return []ContainerLog{}
	}
	type logRequest struct {
		pod       string
		container ContainerState
		previous  bool
		priority  int
	}
	var requests []logRequest
	for _, pod := range pods {
		for _, container := range append(append([]ContainerState{}, pod.InitContainers...), pod.Containers...) {
			requests = append(requests, logRequest{
				pod:       pod.Name,
				container: container,
				priority:  logPriority(container, false),
			})
			if container.RestartCount > 0 {
				requests = append(requests, logRequest{
					pod:       pod.Name,
					container: container,
					previous:  true,
					priority:  logPriority(container, true),
				})
			}
		}
	}
	sort.SliceStable(requests, func(i, j int) bool {
		if requests[i].priority != requests[j].priority {
			return requests[i].priority < requests[j].priority
		}
		if requests[i].pod != requests[j].pod {
			return requests[i].pod < requests[j].pod
		}
		if requests[i].container.Name != requests[j].container.Name {
			return requests[i].container.Name < requests[j].container.Name
		}
		return requests[i].previous && !requests[j].previous
	})
	if len(requests) > defaultMaxLogStreams {
		s.Warnings = append(s.Warnings, fmt.Sprintf("container logs were truncated from %d to %d prioritized streams", len(requests), defaultMaxLogStreams))
		requests = requests[:defaultMaxLogStreams]
	}

	var logs []ContainerLog
	for _, request := range requests {
		args := []string{
			"-n", namespace, "logs", request.pod, "-c", request.container.Name,
			"--tail=" + strconv.Itoa(opts.TailLines),
			"--limit-bytes=" + strconv.Itoa(opts.LogLimitBytes),
		}
		if request.previous {
			args = append(args, "--previous")
		}
		out, err := runner.Raw(ctx, args, nil)
		entry := ContainerLog{Pod: request.pod, Container: request.container.Name, Previous: request.previous}
		if request.container.Name == raylogoffload.SidecarContainerName {
			entry.Role = "ray-driver"
		}
		resource := "pods/" + request.pod + "/logs/" + request.container.Name
		if request.previous {
			resource += "?previous=true"
		}
		if err != nil {
			status, message := classifyError(err, request.pod, false)
			entry.Status, entry.Message = status, message
			s.Access = append(s.Access, Access{Resource: resource, Status: status, Message: message})
		} else {
			entry.Status = "available"
			entry.Text = truncateText(Redact(out), opts.LogLimitBytes)
			s.Access = append(s.Access, Access{Resource: resource, Status: "available"})
		}
		logs = append(logs, entry)
	}
	return logs
}

func logPriority(container ContainerState, previous bool) int {
	if container.Name == raylogoffload.SidecarContainerName {
		return 0
	}
	if previous || container.RestartCount > 0 ||
		container.Current.State == "terminated" ||
		container.Last.State == "terminated" {
		return 1
	}
	if container.Current.State == "waiting" {
		return 2
	}
	return 3
}

func addRunIdentity(s *Snapshot, obj genericObject) {
	s.Run.Kinds = append(s.Run.Kinds, obj.Kind)
	if obj.Metadata.UID != "" {
		s.Run.UIDs = append(s.Run.UIDs, obj.Metadata.UID)
	}
	if s.Run.Labels == nil {
		s.Run.Labels = safeMetadata(obj.Metadata.Labels)
	}
	if s.Run.Annotations == nil {
		s.Run.Annotations = safeMetadata(obj.Metadata.Annotations)
	}
}

func summarizeWorkloadObject(obj genericObject, fallbackKind string) WorkloadObject {
	kind := obj.Kind
	if kind == "" {
		kind = fallbackKind
	}
	specKeys := []string{"suspend", "parallelism", "completions", "backoffLimit", "managedBy", "ttlSecondsAfterFinished", "shutdownAfterJobFinishes"}
	statusKeys := []string{
		"active", "succeeded", "failed", "ready",
		"startTime", "completionTime", "endTime",
		"jobDeploymentStatus", "jobStatus", "jobId",
		"rayClusterName", "rayClusterStatus",
		"state", "reason", "message", "conditions",
		"desiredWorkerReplicas", "readyWorkerReplicas", "availableWorkerReplicas",
		"minWorkerReplicas", "maxWorkerReplicas",
	}
	return WorkloadObject{
		ObjectRef: ObjectRef{
			APIVersion: obj.APIVersion,
			Kind:       kind,
			Name:       obj.Metadata.Name,
			UID:        obj.Metadata.UID,
		},
		Namespace:   obj.Metadata.Namespace,
		CreatedAt:   obj.Metadata.CreationTimestamp,
		Generation:  obj.Metadata.Generation,
		Labels:      safeMetadata(obj.Metadata.Labels),
		Annotations: safeMetadata(obj.Metadata.Annotations),
		Owners:      ownerRefs(obj.Metadata),
		Spec:        selectedMap(obj.Spec, specKeys...),
		Status:      selectedMap(obj.Status, statusKeys...),
	}
}

func summarizeWorkload(obj genericObject) KueueWorkload {
	status := sanitizeMap(obj.Status)
	return KueueWorkload{
		ObjectRef: ObjectRef{
			APIVersion: obj.APIVersion,
			Kind:       first(obj.Kind, "Workload"),
			Name:       obj.Metadata.Name,
			UID:        obj.Metadata.UID,
		},
		Namespace:         obj.Metadata.Namespace,
		CreatedAt:         obj.Metadata.CreationTimestamp,
		Owners:            ownerRefs(obj.Metadata),
		QueueName:         nestedString(obj.Spec, "queueName"),
		PriorityClassName: nestedString(obj.Spec, "priorityClassName"),
		Admission:         nestedMap(status, "admission"),
		AdmissionChecks:   mapSlice(status["admissionChecks"]),
		Conditions:        conditions(status["conditions"]),
	}
}

func summarizePod(obj genericObject) Pod {
	specContainers := containerImages(obj.Spec)
	status := sanitizeMap(obj.Status)
	return Pod{
		ObjectRef: ObjectRef{
			APIVersion: obj.APIVersion,
			Kind:       first(obj.Kind, "Pod"),
			Name:       obj.Metadata.Name,
			UID:        obj.Metadata.UID,
		},
		Namespace:      obj.Metadata.Namespace,
		CreatedAt:      obj.Metadata.CreationTimestamp,
		Owners:         ownerRefs(obj.Metadata),
		Phase:          nestedString(status, "phase"),
		NodeName:       nestedString(obj.Spec, "nodeName"),
		Conditions:     conditions(status["conditions"]),
		InitContainers: containerStates(status["initContainerStatuses"], specContainers),
		Containers:     containerStates(status["containerStatuses"], specContainers),
	}
}

func tauOwnedRoot(meta metadata, name string) bool {
	if strings.EqualFold(meta.Labels[workloadmeta.LabelManagedBy], workloadmeta.ManagedByValue) {
		return true
	}
	for _, key := range []string{workloadmeta.LabelJob, workloadmeta.LabelRun, workloadmeta.LabelRunID} {
		if meta.Labels[key] == name {
			return true
		}
	}
	return false
}

func ownsRun(meta metadata, name string, rootUIDs map[string]bool) bool {
	if len(rootUIDs) > 0 {
		if uid := first(meta.Labels["kueue.x-k8s.io/job-uid"], meta.Annotations["kueue.x-k8s.io/job-uid"]); rootUIDs[uid] {
			return true
		}
		for _, owner := range meta.OwnerReferences {
			if rootUIDs[owner.UID] {
				return true
			}
		}
		return false
	}
	return tauOwnedRoot(meta, name) || meta.Labels[workloadmeta.LabelJob] == name
}

func ownedByUID(meta metadata, ownerUID string) bool {
	if ownerUID == "" {
		return false
	}
	for _, owner := range meta.OwnerReferences {
		if owner.UID == ownerUID {
			return true
		}
	}
	return false
}

func staleDescendantWarning(obj genericObject, rootUIDs map[string]bool) string {
	kind := first(obj.Kind, "object")
	return fmt.Sprintf(
		"%s %s was excluded because its owner UID does not match the current run roots %s",
		kind,
		obj.Metadata.Name,
		strings.Join(sortedKeys(rootUIDs), ","),
	)
}

func sortedKeys(values map[string]bool) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		if key != "" {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	return keys
}

func cloneSet(values map[string]bool) map[string]bool {
	out := make(map[string]bool, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func objectSortKey(raw json.RawMessage) string {
	var header struct {
		Metadata metadata `json:"metadata"`
	}
	if json.Unmarshal(raw, &header) != nil {
		return string(raw)
	}
	return header.Metadata.Namespace + "/" + header.Metadata.Name + "/" + header.Metadata.UID
}

func ownerRefs(meta metadata) []ObjectRef {
	refs := make([]ObjectRef, 0, len(meta.OwnerReferences))
	for _, ref := range meta.OwnerReferences {
		refs = append(refs, ObjectRef{APIVersion: ref.APIVersion, Kind: ref.Kind, Name: ref.Name, UID: ref.UID})
	}
	return refs
}

var diagnosticMetadataKeys = map[string]bool{
	workloadmeta.LabelManagedBy:                  true,
	workloadmeta.LabelWorkspace:                  true,
	workloadmeta.LabelJob:                        true,
	workloadmeta.LabelRun:                        true,
	workloadmeta.LabelRunID:                      true,
	workloadmeta.LabelWorkloadKind:               true,
	workloadmeta.LabelTeam:                       true,
	workloadmeta.LabelLane:                       true,
	workloadmeta.LabelTopology:                   true,
	workloadmeta.LabelPreset:                     true,
	workloadmeta.LabelGPUClass:                   true,
	workloadmeta.LabelGPUCount:                   true,
	"kueue.x-k8s.io/queue-name":                  true,
	workloadmeta.AnnotationWorkspaceID:           true,
	workloadmeta.AnnotationClusterName:           true,
	workloadmeta.AnnotationControllerVerion:      true,
	workloadmeta.AnnotationNamespace:             true,
	workloadmeta.AnnotationDurableID:             true,
	workloadmeta.AnnotationDurableIDUnderscore:   true,
	workloadmeta.AnnotationClusterQueue:          true,
	workloadmeta.AnnotationResourceFlavor:        true,
	workloadmeta.AnnotationPodPriorityClass:      true,
	workloadmeta.AnnotationWorkloadPriorityClass: true,
	workloadmeta.AnnotationKueueTopology:         true,
	workloadmeta.AnnotationImage:                 true,
	workloadmeta.AnnotationImageDigest:           true,
	workloadmeta.AnnotationConfigHash:            true,
	workloadmeta.AnnotationCodeSHA:               true,
	workloadmeta.AnnotationTauCommand:            true,
	workloadmeta.AnnotationSubmissionID:          true,
}

func safeMetadata(values map[string]string) map[string]string {
	out := map[string]string{}
	keys := make([]string, 0, len(values))
	for key := range values {
		if diagnosticMetadataKeys[key] {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) > MaxStatusItems {
		keys = keys[:MaxStatusItems]
	}
	for _, key := range keys {
		value := values[key]
		if sensitiveKey(key) {
			out[key] = "[REDACTED]"
			continue
		}
		out[key] = redactField(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func selectedMap(values map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for _, key := range keys {
		if value, ok := values[key]; ok {
			out[key] = sanitizeValue(value)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sanitizeMap(values map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > MaxStatusItems {
		keys = keys[:MaxStatusItems]
	}
	out := make(map[string]any, len(keys))
	for _, key := range keys {
		value := values[key]
		if sensitiveKey(key) {
			out[key] = "[REDACTED]"
			continue
		}
		out[key] = sanitizeValue(value)
	}
	return out
}

func sensitiveKey(key string) bool {
	key = strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(key, "-", "_"), ".", "_"))
	for _, part := range []string{
		"password",
		"passwd",
		"pwd",
		"token",
		"secret",
		"credential",
		"api_key",
		"access_key",
		"account_key",
		"connection_string",
		"database_url",
		"authorization",
		"cookie",
		"workloadspec_execution",
	} {
		if strings.Contains(key, part) {
			return true
		}
	}
	return false
}

func sanitizeValue(value any) any {
	switch typed := value.(type) {
	case string:
		return redactField(typed)
	case map[string]any:
		return sanitizeMap(typed)
	case []any:
		if len(typed) > MaxStatusItems {
			typed = typed[:MaxStatusItems]
		}
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = sanitizeValue(typed[i])
		}
		return out
	default:
		return value
	}
}

func nestedString(values map[string]any, key string) string {
	value, _ := values[key].(string)
	return redactField(value)
}

func nestedMap(values map[string]any, key string) map[string]any {
	value, _ := values[key].(map[string]any)
	return sanitizeMap(value)
}

func mapSlice(value any) []map[string]any {
	raw, _ := value.([]any)
	if len(raw) > MaxStatusItems {
		raw = raw[:MaxStatusItems]
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if values, ok := item.(map[string]any); ok {
			out = append(out, sanitizeMap(values))
		}
	}
	return out
}

func conditions(value any) []Condition {
	raw, _ := value.([]any)
	if len(raw) > MaxStatusItems {
		raw = raw[:MaxStatusItems]
	}
	out := make([]Condition, 0, len(raw))
	for _, item := range raw {
		values, _ := item.(map[string]any)
		out = append(out, Condition{
			Type:               nestedString(values, "type"),
			Status:             nestedString(values, "status"),
			Reason:             nestedString(values, "reason"),
			Message:            nestedString(values, "message"),
			LastTransitionTime: parseTime(nestedString(values, "lastTransitionTime")),
		})
	}
	return out
}

func containerImages(spec map[string]any) map[string]string {
	out := map[string]string{}
	for _, field := range []string{"initContainers", "containers"} {
		raw, _ := spec[field].([]any)
		for _, item := range raw {
			values, _ := item.(map[string]any)
			name, image := nestedString(values, "name"), nestedString(values, "image")
			if name != "" {
				out[name] = image
			}
		}
	}
	return out
}

func containerStates(value any, images map[string]string) []ContainerState {
	raw, _ := value.([]any)
	if len(raw) > MaxContainersPerPod {
		raw = raw[:MaxContainersPerPod]
	}
	out := make([]ContainerState, 0, len(raw))
	for _, item := range raw {
		values, _ := item.(map[string]any)
		name := nestedString(values, "name")
		restarts, _ := values["restartCount"].(float64)
		ready, _ := values["ready"].(bool)
		out = append(out, ContainerState{
			Name:         name,
			Image:        first(nestedString(values, "image"), images[name]),
			Ready:        ready,
			RestartCount: int(restarts),
			Current:      stateDetail(values["state"]),
			Last:         stateDetail(values["lastState"]),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func stateDetail(value any) StateDetail {
	states, _ := value.(map[string]any)
	for _, state := range []string{"terminated", "waiting", "running"} {
		details, ok := states[state].(map[string]any)
		if !ok {
			continue
		}
		exitCode, exitOK := numberPointer(details["exitCode"])
		signal, signalOK := numberPointer(details["signal"])
		if !exitOK {
			exitCode = nil
		}
		if !signalOK {
			signal = nil
		}
		return StateDetail{
			State:      state,
			Reason:     nestedString(details, "reason"),
			Message:    nestedString(details, "message"),
			ExitCode:   exitCode,
			Signal:     signal,
			StartedAt:  parseTime(nestedString(details, "startedAt")),
			FinishedAt: parseTime(nestedString(details, "finishedAt")),
		}
	}
	return StateDetail{}
}

func (s StateDetail) IsZero() bool {
	return s.State == "" &&
		s.Reason == "" &&
		s.Message == "" &&
		s.ExitCode == nil &&
		s.Signal == nil &&
		s.StartedAt.IsZero() &&
		s.FinishedAt.IsZero()
}

func numberPointer(value any) (*int32, bool) {
	number, ok := value.(float64)
	if !ok {
		return nil, false
	}
	result := int32(number)
	return &result, true
}

func parseTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339Nano, value)
	return parsed
}

func classifyError(err error, objectName string, customResource bool) (string, string) {
	message := redactField(err.Error())
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "forbidden") || strings.Contains(lower, "cannot get resource"):
		return "forbidden", message
	case strings.Contains(lower, "unauthorized"):
		return "unauthorized", message
	case customResource && (strings.Contains(lower, "doesn't have a resource type") || strings.Contains(lower, "no matches for kind")):
		return "unsupported", message
	case apiObjectNotFound(lower, objectName):
		return "absent", message
	}
	return "error", message
}

func apiObjectNotFound(message, objectName string) bool {
	if objectName == "" {
		return false
	}
	const marker = "error from server (notfound):"
	index := strings.Index(message, marker)
	if index < 0 {
		return false
	}
	apiMessage := message[index+len(marker):]
	quotedName := `"` + strings.ToLower(objectName) + `"`
	return strings.Contains(apiMessage, quotedName) && strings.Contains(apiMessage, " not found")
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = redactField(strings.TrimSpace(value))
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	if len(out) > MaxStatusItems {
		total := len(out)
		out = append(out[:MaxStatusItems], fmt.Sprintf("warnings were truncated from %d to %d", total, MaxStatusItems))
	}
	return out
}

func first(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

var redactPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(authorization\s*:\s*(?:[A-Za-z][A-Za-z0-9_-]*\s+)?)[^\s]+`),
	regexp.MustCompile(`(?im)^((?:set-)?cookie\s*:\s*).*$`),
	regexp.MustCompile(`(?i)(\bbearer\s+)[A-Za-z0-9._~+/=-]+`),
	regexp.MustCompile(`(?i)(["']?(?:password|passwd|pwd|token|secret|credential|pat|personal[_-]?access[_-]?token|api[_-]?key|client[_-]?secret|access[_-]?key|account[_-]?key|secret[_-]?access[_-]?key|connection[_-]?string|database[_-]?url|authorization|cookie|set[_-]?cookie)["']?\s*[:=]\s*)("[^"]*"|'[^']*'|[^\s,;}]+)`),
	regexp.MustCompile(`(?i)([?&](?:sig|token|access_token|code|key|secret|password)=)[^&\s]+`),
	regexp.MustCompile(`(?i)([A-Za-z][A-Za-z0-9+.-]*://[^:/\s]+:)[^@\s]+@`),
	regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
	regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,})\b`),
}

var privateKeyBeginPattern = regexp.MustCompile(`-----BEGIN ([A-Z0-9 ]*PRIVATE KEY)-----`)

// Redact removes common credentials from metadata, events, errors, and logs.
func Redact(value string) string {
	value = redactPrivateKeys(value)
	for i, pattern := range redactPatterns {
		switch i {
		case 5:
			value = pattern.ReplaceAllString(value, "${1}[REDACTED]@")
		case 6, 7:
			value = pattern.ReplaceAllString(value, "[REDACTED]")
		default:
			value = pattern.ReplaceAllString(value, "${1}[REDACTED]")
		}
	}
	return value
}

func redactPrivateKeys(value string) string {
	var out strings.Builder
	for {
		match := privateKeyBeginPattern.FindStringSubmatchIndex(value)
		if match == nil {
			out.WriteString(value)
			return out.String()
		}
		begin := value[match[0]:match[1]]
		keyType := value[match[2]:match[3]]
		endMarker := "-----END " + keyType + "-----"
		out.WriteString(value[:match[0]])
		out.WriteString(begin)
		out.WriteString("\n[REDACTED]")
		remaining := value[match[1]:]
		end := strings.Index(remaining, endMarker)
		if end < 0 {
			return out.String()
		}
		out.WriteString("\n")
		out.WriteString(endMarker)
		value = remaining[end+len(endMarker):]
	}
}

func redactField(value string) string {
	return truncateText(Redact(value), MaxFieldBytes)
}

func truncateText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	cut := limit
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + "...[truncated]"
}
