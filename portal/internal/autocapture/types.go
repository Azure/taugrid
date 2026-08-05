package autocapture

import (
	"context"
	"time"
)

const (
	labelKueueJobUID = "kueue.x-k8s.io/job-uid"
)

type Client interface {
	ListJobs(ctx context.Context, namespace string) ([]Job, error)
	ListWorkloads(ctx context.Context, namespace string) ([]Workload, error)
	ListPods(ctx context.Context, namespace string) ([]Pod, error)
	ListResourceClaims(ctx context.Context, namespace string) ([]ResourceClaim, error)
	ListEvents(ctx context.Context, namespace string) ([]KubernetesEvent, error)
}

type Options struct {
	Namespace  string
	Cluster    string
	Project    string
	RunGroupID string
	Owner      string
}

type Result struct {
	Runs               int `json:"runs"`
	CreatedRuns        int `json:"created_runs"`
	UpdatedRuns        int `json:"updated_runs"`
	CreatedRunContexts int `json:"created_run_contexts"`
	UpdatedRunContexts int `json:"updated_run_contexts"`
	Events             int `json:"events"`
	Tags               int `json:"tags"`
	Reused             int `json:"reused"`
}

type Job struct {
	Namespace   string
	Name        string
	UID         string
	Labels      map[string]string
	Annotations map[string]string

	CreatedAt   time.Time
	StartedAt   time.Time
	FinishedAt  time.Time
	Suspended   bool
	Active      int
	Succeeded   int
	Failed      int
	Parallelism int
	Conditions  []Condition
}

type Condition struct {
	Type               string
	Status             string
	Reason             string
	Message            string
	LastTransitionTime time.Time
}

type Workload struct {
	Namespace    string
	Name         string
	UID          string
	Labels       map[string]string
	Queue        string
	ClusterQueue string
	Admitted     bool
	Phase        string
	Reason       string
	Preemption   string
	AdmittedAt   time.Time
	FinishedAt   time.Time
}

type Pod struct {
	Namespace       string
	Name            string
	UID             string
	Labels          map[string]string
	Phase           string
	Node            string
	Restarts        int
	Ready           string
	StartedAt       time.Time
	ResourceClaims  []string
	ContainerReason string
	ExitCode        *int32
	OOMKilled       bool
}

type ResourceClaim struct {
	Namespace   string
	Name        string
	UID         string
	Labels      map[string]string
	NodeName    string
	DeviceClass string
}

type KubernetesEvent struct {
	Namespace string
	Name      string
	UID       string
	Type      string
	Reason    string
	Action    string
	Message   string
	Source    string
	Count     int32
	Time      time.Time
	Regarding ObjectRef
}

type ObjectRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	UID       string `json:"uid"`
}
