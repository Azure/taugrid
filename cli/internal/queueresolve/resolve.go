package queueresolve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	DefaultLocalQueueLabel = "kueue.x-k8s.io/default-local-queue"
	QueueTeamLabel         = "kueue.x-k8s.io/team"
	DefaultQueueName       = "jobqueue"
)

type ResolveAccessibleQueueOptions struct {
	Namespace        string
	QueueName        string
	Team             string
	WorkloadResource string
}

type AccessibleQueue struct {
	Namespace string
	QueueName string
	Team      string
}

// rejection records why a labelled namespace was not usable, so the failure
// message can name the specific onboarding step that is missing instead of
// collapsing every cause into "no authorized queue namespace".
type rejection struct {
	namespace string
	reason    string
}

type namespaceList struct {
	Items []namespaceItem `json:"items"`
}

type namespaceItem struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
}

// ResolveAccessibleQueue finds the namespace-local queue the current kubectl
// identity can use. Kubernetes RBAC is the source of truth; Tau does not read
// Entra group membership.
func ResolveAccessibleQueue(ctx context.Context, r RawRunner, opts ResolveAccessibleQueueOptions) (AccessibleQueue, []AccessibleQueue, error) {
	resource := strings.TrimSpace(opts.WorkloadResource)
	if resource == "" {
		resource = "rayjobs.ray.io"
	}
	namespaces, err := listQueueNamespaces(ctx, r)
	if err != nil {
		return AccessibleQueue{}, nil, err
	}
	if namespace := strings.TrimSpace(opts.Namespace); namespace != "" {
		var selected []AccessibleQueue
		for _, ns := range namespaces {
			if ns.Namespace == namespace {
				selected = append(selected, ns)
			}
		}
		if len(selected) == 0 {
			return AccessibleQueue{}, nil, fmt.Errorf(
				"namespace %q does not declare a default Kueue LocalQueue with label %s; ask the platform owner to configure the namespace",
				namespace, DefaultLocalQueueLabel,
			)
		}
		namespaces = selected
	}
	var candidates []AccessibleQueue
	var rejected []rejection
	var teamFiltered int
	for _, ns := range namespaces {
		if !teamMatches(opts.Team, ns.Team, ns.Namespace) {
			teamFiltered++
			continue
		}
		queueName := strings.TrimSpace(opts.QueueName)
		if queueName == "" || strings.EqualFold(queueName, "auto") {
			queueName = ns.QueueName
		}
		if queueName == "" {
			queueName = DefaultQueueName
		}
		allowed, err := canI(ctx, r, "create", resource, ns.Namespace)
		if err != nil {
			rejected = append(rejected, rejection{ns.Namespace,
				fmt.Sprintf("authorization check for create %s failed: %s", resource, firstErrorLine(err))})
			continue
		}
		if !allowed {
			rejected = append(rejected, rejection{ns.Namespace,
				fmt.Sprintf("not authorized to create %s (RBAC)", resource)})
			continue
		}
		allowed, err = canI(ctx, r, "get", "localqueues.kueue.x-k8s.io", ns.Namespace)
		if err != nil {
			rejected = append(rejected, rejection{ns.Namespace,
				"authorization check for get localqueues.kueue.x-k8s.io failed: " + firstErrorLine(err)})
			continue
		}
		if !allowed {
			rejected = append(rejected, rejection{ns.Namespace,
				"not authorized to get localqueues.kueue.x-k8s.io (RBAC)"})
			continue
		}
		if _, err := getLocalQueue(ctx, r, ns.Namespace, queueName); err != nil {
			reason := fmt.Sprintf("cannot read LocalQueue %q: %s", queueName, firstErrorLine(err))
			if isNotFoundError(err) {
				reason = fmt.Sprintf("LocalQueue %q not found", queueName)
			}
			rejected = append(rejected, rejection{ns.Namespace, reason})
			continue
		}
		ns.QueueName = queueName
		candidates = append(candidates, ns)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Namespace != candidates[j].Namespace {
			return candidates[i].Namespace < candidates[j].Namespace
		}
		return candidates[i].QueueName < candidates[j].QueueName
	})
	if len(candidates) == 1 {
		return candidates[0], candidates, nil
	}
	if len(candidates) == 0 {
		return AccessibleQueue{}, candidates, unresolvedQueueError(resource, opts.Team, len(namespaces), teamFiltered, rejected)
	}
	return AccessibleQueue{}, candidates, fmt.Errorf("multiple authorized Tau queue namespaces found; pass --namespace or --team to disambiguate")
}

// unresolvedQueueError explains which onboarding precondition actually failed.
// The three causes are very different fixes: the cluster was never onboarded,
// the caller lacks RBAC, or the workspace's LocalQueue has not reconciled yet.
func unresolvedQueueError(resource, team string, labelled, teamFiltered int, rejected []rejection) error {
	var b strings.Builder
	fmt.Fprintf(&b, "no usable Tau queue namespace found for %s", resource)

	switch {
	case labelled == 0:
		b.WriteString("\n  cause: no namespace carries the " + DefaultLocalQueueLabel + " label")
		b.WriteString("\n  fix:   this cluster has no Tau workspace yet. Ask the cluster admin to create a")
		b.WriteString("\n         TauWorkspace (the controller creates the namespace, its LocalQueue, and this label).")
	case len(rejected) == 0 && teamFiltered > 0:
		fmt.Fprintf(&b, "\n  cause: %d queue namespace(s) exist but none match --team %q", teamFiltered, team)
		b.WriteString("\n  fix:   drop --team, or pass a team that matches the namespace's " + QueueTeamLabel + " label.")
	default:
		fmt.Fprintf(&b, "\n  cause: %d queue namespace(s) exist but none are usable by your identity:", labelled)
		for _, r := range rejected {
			fmt.Fprintf(&b, "\n         - %s: %s", r.namespace, r.reason)
		}
		b.WriteString("\n  fix:   RBAC denials mean you are not bound to the workspace - ask the cluster admin to add you")
		b.WriteString("\n         to its TauWorkspace subject. A missing LocalQueue means the workspace has not reconciled;")
		b.WriteString("\n         check: kubectl get workspaces.tau.azure.com -n tau-platform")
		if teamFiltered > 0 {
			fmt.Fprintf(&b, "\n  note:  %d further namespace(s) were excluded by --team %q.", teamFiltered, team)
		}
	}
	return errors.New(b.String())
}

func listQueueNamespaces(ctx context.Context, r RawRunner) ([]AccessibleQueue, error) {
	out, err := r.Raw(ctx, []string{"get", "namespaces", "-l", DefaultLocalQueueLabel, "-o", "json"}, nil)
	if err != nil {
		return nil, fmt.Errorf("list queue namespaces: %w", err)
	}
	var list namespaceList
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parse queue namespaces: %w", err)
	}
	namespaces := make([]AccessibleQueue, 0, len(list.Items))
	for _, item := range list.Items {
		name := strings.TrimSpace(item.Metadata.Name)
		if name == "" {
			continue
		}
		namespaces = append(namespaces, AccessibleQueue{
			Namespace: name,
			QueueName: strings.TrimSpace(item.Metadata.Labels[DefaultLocalQueueLabel]),
			Team:      strings.TrimSpace(item.Metadata.Labels[QueueTeamLabel]),
		})
	}
	return namespaces, nil
}

func canI(ctx context.Context, r RawRunner, verb, resource, namespace string) (bool, error) {
	out, err := r.Raw(ctx, []string{"auth", "can-i", verb, resource, "-n", namespace}, nil)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) == "yes", nil
}

func isNotFoundError(err error) bool {
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "notfound") || strings.Contains(text, "not found")
}

func firstErrorLine(err error) string {
	text := strings.TrimSpace(err.Error())
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return strings.TrimSpace(text[:i])
	}
	return text
}

func teamMatches(requested, label, namespace string) bool {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return true
	}
	label = strings.TrimSpace(label)
	if requested == label {
		return true
	}
	return strings.HasSuffix(namespace, "-"+requested) || namespace == "team-"+requested
}
