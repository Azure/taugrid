package status

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Azure/taugrid/core/kube"
	"github.com/Azure/taugrid/core/kueueapi"
	"github.com/Azure/taugrid/core/workloadmeta"
)

type rawRunner interface {
	Raw(ctx context.Context, extraArgs []string, stdin []byte) (string, error)
}

// FetchRunLogs populates the minimal Snapshot surface `tau run logs` needs to
// decide between local Job/RayJob logs and manager-side MultiKueue ADX logs.
// Unlike Fetch, it never queries Pods, Events, ResourceClaims, or
// AdmissionChecks. Real kubectl/query failures are returned alongside any
// successfully hydrated partial snapshot so the CLI can surface authoritative
// manager-side placement errors without widening RBAC.
func FetchRunLogs(ctx context.Context, r *kube.Runner, namespace, name string) (Snapshot, error) {
	return fetchRunLogs(ctx, r, namespace, name)
}

func fetchRunLogs(ctx context.Context, r rawRunner, namespace, name string) (Snapshot, error) {
	s := Snapshot{Name: name, Namespace: namespace}
	var firstErr error

	jobJSON, jobFound, err := fetchManagerCleanupObject(ctx, r, namespace, "job", name, false, "job.batch", "jobs.batch")
	if err != nil && firstErr == nil {
		firstErr = fmt.Errorf("get job %s/%s while resolving run logs placement: %w", namespace, name, err)
	}
	if jobFound {
		s.JobFound = true
		hydrateJob(&s, []byte(jobJSON))
	}

	rayJobJSON, rayJobFound, err := fetchManagerCleanupObject(ctx, r, namespace, "rayjob", name, true, "rayjob.ray.io", "rayjobs.ray.io")
	if err != nil && firstErr == nil {
		firstErr = fmt.Errorf("get RayJob %s/%s while resolving run logs placement: %w", namespace, name, err)
	}
	if rayJobFound {
		hydrateRayJob(&s, []byte(rayJobJSON))
	}

	workloads, err := fetchManagerCleanupWorkloads(ctx, r, namespace, name, s.JobUID, s.RayJob.UID)
	if err != nil && firstErr == nil {
		firstErr = fmt.Errorf("list Kueue Workloads for %s/%s while resolving run logs placement: %w", namespace, name, err)
	}
	s.Workloads = workloads
	markRunLogsMultiKueueAdmissionCheckFallbacks(s.Workloads)

	if err := ctx.Err(); err != nil && firstErr == nil {
		firstErr = err
	}
	return s, firstErr
}

// Fetch populates a Snapshot for one job by name+namespace using the
// supplied kube.Runner. Soft-fails (returns partial Snapshot) when
// individual sub-queries fail — `tau run status` should always show what
// it can rather than abort on a missing CRD.
func Fetch(ctx context.Context, r *kube.Runner, namespace, name string) (Snapshot, error) {
	s := Snapshot{Name: name, Namespace: namespace}

	// Job: -o json so we get conditions + status counts.
	jobJSON, err := r.Raw(ctx, []string{"-n", namespace, "get", "job", name, "-o", "json"}, nil)
	if err == nil {
		s.JobFound = true
		hydrateJob(&s, []byte(jobJSON))
	}
	// Whether Job exists or not, still try RayJob + Workloads + Pods.
	rayJobJSON, err := r.Raw(ctx, []string{"-n", namespace, "get", "rayjob", name, "-o", "json"}, nil)
	if err == nil {
		hydrateRayJob(&s, []byte(rayJobJSON))
	}

	// Workloads. Kueue labels the Workload with kueue.x-k8s.io/job-uid =
	// the Job's metadata.uid, AND we add tau.azure.com/job=<name> via
	// the workload metadata/template labels. Either selector should work; we prefer
	// our own label since it survives Job recreation.
	wlJSON, err := r.Raw(ctx, []string{"-n", namespace, "get", "workloads.kueue.x-k8s.io",
		"-l", workloadmeta.LabelJob + "=" + name, "-o", "json"}, nil)
	if err == nil {
		s.Workloads = hydrateWorkloads([]byte(wlJSON))
	}
	// Fall back to job-uid selector if our label found nothing.
	if len(s.Workloads) == 0 && s.JobFound {
		uid := jobUID([]byte(jobJSON))
		if uid != "" {
			wlJSON2, err2 := r.Raw(ctx, []string{"-n", namespace, "get", "workloads.kueue.x-k8s.io",
				"-l", "kueue.x-k8s.io/job-uid=" + uid, "-o", "json"}, nil)
			if err2 == nil {
				s.Workloads = hydrateWorkloads([]byte(wlJSON2))
			}
		}
	}
	rj := snapshotRayJob(s)
	if len(s.Workloads) == 0 && rj.Found && rj.UID != "" {
		wlJSON2, err2 := r.Raw(ctx, []string{"-n", namespace, "get", "workloads.kueue.x-k8s.io",
			"-l", "kueue.x-k8s.io/job-uid=" + rj.UID, "-o", "json"}, nil)
		if err2 == nil {
			s.Workloads = hydrateWorkloads([]byte(wlJSON2))
		}
	}
	hydrateAdmissionCheckControllers(ctx, r, s.Workloads)

	// Pods: combine selectors. job-name is the standard k8s Job label;
	// tau.azure.com/job is ours, and Ray pods also carry ray.io/cluster.
	selectors := []string{"job-name=" + name, workloadmeta.LabelJob + "=" + name}
	if rj.RayClusterName != "" {
		selectors = append(selectors, "ray.io/cluster="+rj.RayClusterName)
	}
	for _, selector := range selectors {
		podJSON, err := r.Raw(ctx, []string{"-n", namespace, "get", "pods",
			"-l", selector, "-o", "json"}, nil)
		if err == nil {
			s.PodsObserved = true
			s.Pods = mergePods(s.Pods, hydratePods([]byte(podJSON)))
		}
	}

	s.ResourceClaims = fetchResourceClaims(ctx, r, namespace, uniquePodResourceClaims(s.Pods))
	s.Events = fetchEvents(ctx, r, namespace, eventObjectNames(s))

	return s, nil
}

// FetchManagerCleanup populates the subset of Snapshot needed by manager-side
// cleanup logic before delete. Unlike Fetch, every manager-cluster query is
// authoritative: exact object NotFound and missing RayJob CRDs are tolerated,
// but real kubectl/query failures are surfaced while still preserving any
// successfully hydrated partial snapshot.
func FetchManagerCleanup(ctx context.Context, r rawRunner, namespace, name string) (Snapshot, error) {
	s := Snapshot{Name: name, Namespace: namespace}
	var firstErr error

	jobJSON, jobFound, err := fetchManagerCleanupObject(ctx, r, namespace, "job", name, false, "job.batch", "jobs.batch")
	if err != nil && firstErr == nil {
		firstErr = err
	}
	if jobFound {
		s.JobFound = true
		hydrateJob(&s, []byte(jobJSON))
	}

	rayJobJSON, rayJobFound, err := fetchManagerCleanupObject(ctx, r, namespace, "rayjob", name, true, "rayjob.ray.io", "rayjobs.ray.io")
	if err != nil && firstErr == nil {
		firstErr = err
	}
	if rayJobFound {
		hydrateRayJob(&s, []byte(rayJobJSON))
	}

	workloads, err := fetchManagerCleanupWorkloads(ctx, r, namespace, name, s.JobUID, s.RayJob.UID)
	if err != nil && firstErr == nil {
		firstErr = err
	}
	s.Workloads = workloads
	hydrateAdmissionCheckControllers(ctx, r, s.Workloads)

	if err := ctx.Err(); err != nil && firstErr == nil {
		firstErr = err
	}
	return s, firstErr
}

func fetchManagerCleanupObject(ctx context.Context, r rawRunner, namespace, resource, name string, allowUnknownResource bool, resourceKinds ...string) (string, bool, error) {
	out, err := r.Raw(ctx, []string{"-n", namespace, "get", resource, name, "-o", "json"}, nil)
	switch {
	case err == nil:
		return out, true, nil
	case allowUnknownResource && cleanupUnknownResourceError(err):
		return "", false, nil
	case cleanupExactObjectNotFound(err, name, resourceKinds...):
		return "", false, nil
	default:
		return "", false, err
	}
}

func fetchManagerCleanupWorkloads(ctx context.Context, r rawRunner, namespace, name, jobUID, rayJobUID string) ([]Workload, error) {
	var (
		workloads []Workload
		firstErr  error
	)
	for _, selector := range managerCleanupWorkloadSelectors(name, jobUID, rayJobUID) {
		selected, err := fetchManagerCleanupWorkloadsBySelector(ctx, r, namespace, selector)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		workloads = mergeManagerCleanupWorkloads(workloads, selected)
	}
	return workloads, firstErr
}

func fetchManagerCleanupWorkloadsBySelector(ctx context.Context, r rawRunner, namespace, selector string) ([]Workload, error) {
	out, err := r.Raw(ctx, []string{"-n", namespace, "get", "workloads.kueue.x-k8s.io", "-l", selector, "-o", "json"}, nil)
	if err != nil {
		return nil, err
	}
	return hydrateWorkloads([]byte(out)), nil
}

func managerCleanupWorkloadSelectors(name, jobUID, rayJobUID string) []string {
	selectors := make([]string, 0, 3)
	if name != "" {
		selectors = append(selectors, workloadmeta.LabelJob+"="+name)
	}
	if jobUID != "" {
		selectors = append(selectors, "kueue.x-k8s.io/job-uid="+jobUID)
	}
	if rayJobUID != "" {
		selectors = append(selectors, "kueue.x-k8s.io/job-uid="+rayJobUID)
	}
	seen := make(map[string]bool, len(selectors))
	out := make([]string, 0, len(selectors))
	for _, selector := range selectors {
		if selector == "" || seen[selector] {
			continue
		}
		seen[selector] = true
		out = append(out, selector)
	}
	return out
}

func mergeManagerCleanupWorkloads(existing, incoming []Workload) []Workload {
	if len(incoming) == 0 {
		return existing
	}
	seen := make(map[string]bool, len(existing)+len(incoming))
	out := make([]Workload, 0, len(existing)+len(incoming))
	for _, workload := range existing {
		out = append(out, workload)
		if workload.Name != "" {
			seen[workload.Name] = true
		}
	}
	for _, workload := range incoming {
		if workload.Name != "" && seen[workload.Name] {
			continue
		}
		out = append(out, workload)
		if workload.Name != "" {
			seen[workload.Name] = true
		}
	}
	return out
}

// --- JSON shapes (subset of what we need) ---

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
	if len(s.Labels) == 0 {
		s.Labels = rj.Metadata.Labels
	}
	if len(s.Annotations) == 0 {
		s.Annotations = rj.Metadata.Annotations
	}
}

func jobUID(data []byte) string {
	var j jobObj
	if err := json.Unmarshal(data, &j); err != nil {
		return ""
	}
	return j.Metadata.UID
}

type wlList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			QueueName string `json:"queueName"`
		} `json:"spec"`
		Status struct {
			Conditions []kueueapi.Condition `json:"conditions"`
			// MultiKueue fields. All are manager-visible: Kueue's
			// multikueue admission check controller writes these on the
			// manager cluster's Workload object; nothing here requires
			// reading worker cluster state or credentials.
			ClusterName           string   `json:"clusterName"`
			NominatedClusterNames []string `json:"nominatedClusterNames"`
			AdmissionChecks       []struct {
				Name               string    `json:"name"`
				State              string    `json:"state"`
				Message            string    `json:"message"`
				LastTransitionTime time.Time `json:"lastTransitionTime"`
			} `json:"admissionChecks"`
		} `json:"status"`
	} `json:"items"`
}

type admissionCheckObj struct {
	Spec struct {
		ControllerName string `json:"controllerName"`
	} `json:"spec"`
}

type admissionCheckLookup struct {
	ControllerName string
	LookupFailed   bool
}

func hydrateWorkloads(data []byte) []Workload {
	var l wlList
	if err := json.Unmarshal(data, &l); err != nil {
		return nil
	}
	out := make([]Workload, 0, len(l.Items))
	for _, it := range l.Items {
		w := Workload{Name: it.Metadata.Name, Queue: it.Spec.QueueName, Phase: "Pending"}
		for _, c := range it.Status.Conditions {
			switch c.Type {
			case "Admitted":
				if c.Status == "True" {
					w.Admitted = true
					if w.Phase == "Pending" {
						w.Phase = "Admitted"
					}
				}
			case "Finished":
				if c.Status == "True" {
					w.Phase = "Finished"
					if c.Reason != "" {
						w.Reason = c.Reason
					}
				}
			case "QuotaReserved":
				if c.Status == "True" && w.Phase == "Pending" {
					w.Phase = "QuotaReserved"
				}
			}
			if strings.Contains(strings.ToLower(c.Type), "evict") || strings.Contains(strings.ToLower(c.Reason), "preempt") {
				w.Preemption = firstNonEmpty(c.Reason, c.Message, c.Type)
			}
		}
		// Only meaningful while Kueue is still deciding; once the workload
		// is admitted or finished these describe a past state, and Finished
		// has already set its own Reason.
		if w.waiting() {
			w.Reason, w.Message = kueueapi.PendingCause(it.Status.Conditions)
		}
		w.ClusterName = it.Status.ClusterName
		w.NominatedClusterNames = sortedUniqueStrings(it.Status.NominatedClusterNames)
		for _, ac := range it.Status.AdmissionChecks {
			if ac.Name == "" {
				continue
			}
			w.AdmissionChecks = append(w.AdmissionChecks, AdmissionCheck{
				Name:               ac.Name,
				State:              ac.State,
				Message:            ac.Message,
				LastTransitionTime: ac.LastTransitionTime,
			})
		}
		sort.Slice(w.AdmissionChecks, func(i, j int) bool {
			return w.AdmissionChecks[i].Name < w.AdmissionChecks[j].Name
		})
		out = append(out, w)
	}
	return out
}

func hydrateAdmissionCheckControllers(ctx context.Context, r rawRunner, workloads []Workload) {
	if r == nil || len(workloads) == 0 {
		return
	}
	controllers := fetchAdmissionCheckControllers(ctx, r, admissionCheckNames(workloads))
	if len(controllers) == 0 {
		return
	}
	for wi := range workloads {
		for ci := range workloads[wi].AdmissionChecks {
			lookup, ok := controllers[workloads[wi].AdmissionChecks[ci].Name]
			if !ok {
				continue
			}
			workloads[wi].AdmissionChecks[ci].ControllerName = lookup.ControllerName
			workloads[wi].AdmissionChecks[ci].ControllerLookupFailed = lookup.LookupFailed
		}
	}
}

// markRunLogsMultiKueueAdmissionCheckFallbacks preserves the pinned exact-name
// MultiKueue fallback for minimal run-logs snapshots, which intentionally skip
// AdmissionCheck object lookups to match the read-only manager viewer RBAC.
func markRunLogsMultiKueueAdmissionCheckFallbacks(workloads []Workload) {
	for wi := range workloads {
		for ci := range workloads[wi].AdmissionChecks {
			if workloads[wi].AdmissionChecks[ci].Name != multiKueueAdmissionCheckName {
				continue
			}
			if strings.TrimSpace(workloads[wi].AdmissionChecks[ci].ControllerName) != "" {
				continue
			}
			workloads[wi].AdmissionChecks[ci].ControllerLookupFailed = true
		}
	}
}

func admissionCheckNames(workloads []Workload) []string {
	seen := make(map[string]bool)
	names := make([]string, 0)
	for _, workload := range workloads {
		for _, check := range workload.AdmissionChecks {
			if check.Name == "" || seen[check.Name] {
				continue
			}
			seen[check.Name] = true
			names = append(names, check.Name)
		}
	}
	sort.Strings(names)
	return names
}

func fetchAdmissionCheckControllers(ctx context.Context, r rawRunner, names []string) map[string]admissionCheckLookup {
	controllers := make(map[string]admissionCheckLookup, len(names))
	for _, name := range names {
		out, err := r.Raw(ctx, []string{"get", "admissioncheck", name, "-o", "json"}, nil)
		if err != nil {
			controllers[name] = admissionCheckLookup{LookupFailed: true}
			continue
		}
		var obj admissionCheckObj
		if err := json.Unmarshal([]byte(out), &obj); err != nil {
			controllers[name] = admissionCheckLookup{LookupFailed: true}
			continue
		}
		controllers[name] = admissionCheckLookup{ControllerName: strings.TrimSpace(obj.Spec.ControllerName)}
	}
	return controllers
}

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

func sortedUniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
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

func cleanupUnknownResourceError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "the server doesn't have a resource type") ||
		strings.Contains(msg, "server doesn't have a resource type") ||
		strings.Contains(msg, "no matches for kind") ||
		strings.Contains(msg, "resource type") && strings.Contains(msg, "not found")
}

func cleanupExactObjectNotFound(err error, name string, resourceKinds ...string) bool {
	if err == nil || name == "" {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	quotedName := `"` + strings.ToLower(name) + `"`
	for _, resourceKind := range resourceKinds {
		pattern := strings.ToLower(resourceKind) + " " + quotedName + " not found"
		if strings.Contains(msg, pattern) {
			return true
		}
	}
	return false
}
