package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

type phase struct {
	mark, name, detail string
}

type runView struct {
	header string
	phases []phase
	final  bool
}

func runStatus(ctx context.Context, c *clients, args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	ns := fs.String("n", "team-a", "workspace namespace")
	watch := fs.Bool("watch", false, "refresh until the run succeeds")
	pos := parseWithPositional(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("usage: tau-demo status <job-name> [-n ns] [--watch]")
	}
	name := pos[0]
	for {
		v, err := view(ctx, c, *ns, name)
		if err != nil {
			return err
		}
		if *watch {
			clearScreen()
		}
		v.print()
		if !*watch || v.final {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func (v runView) print() {
	fmt.Println(v.header)
	fmt.Println("Startup phases:")
	for _, p := range v.phases {
		fmt.Printf("  %s %-22s %s\n", p.mark, p.name, p.detail)
	}
}

func view(ctx context.Context, c *clients, ns, name string) (runView, error) {
	job, err := c.core.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return runView{header: fmt.Sprintf("Run %s/%s: not found (Job TTL deleted it after success)", ns, name), final: true}, nil
	}
	if err != nil {
		return runView{}, err
	}
	v := runView{header: fmt.Sprintf("Run %s/%s  profile=%s  queue=%s  %s", ns, name,
		job.Labels[labelProfile], job.Labels[labelQueue], time.Now().Format("15:04:05"))}
	v.add("✓", "Submitted", "Job applied (suspended)")

	wl, err := workloadFor(ctx, c, ns, string(job.UID))
	if err != nil {
		return v, err
	}
	if wl == nil {
		v.add("…", "Quota reserved", "waiting for Kueue to create the Workload")
		return v, nil
	}

	conds := conditions(wl)
	reserved := conds["QuotaReserved"]
	if reserved.Status != "True" {
		v.add("…", "Quota reserved", "queued: "+firstLine(reserved.Message))
		v.add("·", "Capacity provisioning", "")
		v.add("·", "Pod scheduling", "")
		v.add("·", "Running", "")
		return v, nil
	}
	v.add("✓", "Quota reserved", quotaDetail(wl))

	if !isTrue(wl, "Admitted") {
		v.add("…", "Capacity provisioning", capacityDetail(ctx, c, wl, reserved))
		v.add("·", "Pod scheduling", "")
		v.add("·", "Running", "")
		return v, nil
	}
	detail := "nodes provisioned"
	if pr, _ := provisioningRequestFor(ctx, c, wl); pr != nil {
		if msg := conditions(pr)["Provisioned"].Message; msg != "" {
			detail = msg
		}
	}
	v.add("✓", "Capacity provisioning", detail)

	pods, err := c.core.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "batch.kubernetes.io/job-name=" + name})
	if err != nil {
		return v, err
	}
	want := int32(1)
	if job.Spec.Parallelism != nil {
		want = *job.Spec.Parallelism
	}
	scheduled, nodes := 0, map[string]bool{}
	for _, p := range pods.Items {
		if p.Spec.NodeName != "" {
			scheduled++
			nodes[p.Spec.NodeName] = true
		}
	}
	switch {
	case succeeded(job):
		v.add("✓", "Pod scheduling", fmt.Sprintf("%d pod(s) placed", want))
		v.add("✓", "Running", fmt.Sprintf("Succeeded (%d/%d completions)", job.Status.Succeeded, want))
		v.final = true
	case scheduled < int(want):
		v.add("…", "Pod scheduling", fmt.Sprintf("%d/%d pods placed", scheduled, want))
		v.add("·", "Running", "")
	default:
		v.add("✓", "Pod scheduling", fmt.Sprintf("%d/%d on %s", scheduled, want, joinKeys(nodes)))
		v.add("…", "Running", fmt.Sprintf("%d active", job.Status.Active))
	}
	return v, nil
}

func (v *runView) add(mark, name, detail string) {
	v.phases = append(v.phases, phase{mark, name, detail})
}

func quotaDetail(wl *unstructured.Unstructured) string {
	cq, _, _ := unstructured.NestedString(wl.Object, "status", "admission", "clusterQueue")
	assignments, _, _ := unstructured.NestedSlice(wl.Object, "status", "admission", "podSetAssignments")
	var gpu, flavor string
	for _, item := range assignments {
		m, _ := item.(map[string]any)
		usage, _, _ := unstructured.NestedStringMap(m, "resourceUsage")
		flavors, _, _ := unstructured.NestedStringMap(m, "flavors")
		if g := usage["nvidia.com/gpu"]; g != "" {
			gpu, flavor = g, flavors["nvidia.com/gpu"]
		}
	}
	return fmt.Sprintf("%s nvidia.com/gpu on ClusterQueue %s (flavor %s)", gpu, cq, flavor)
}

func capacityDetail(ctx context.Context, c *clients, wl *unstructured.Unstructured, reserved condition) string {
	detail := "waiting for Cluster Autoscaler"
	if pr, _ := provisioningRequestFor(ctx, c, wl); pr != nil {
		p := conditions(pr)["Provisioned"]
		if p.Reason != "" {
			detail = fmt.Sprintf("%s: %s", p.Reason, p.Message)
		} else {
			detail = "ProvisioningRequest " + pr.GetName() + " created"
		}
	}
	elapsed := time.Since(reserved.LastTransition).Round(time.Second)
	return fmt.Sprintf("%s (%s)", detail, elapsed)
}

func succeeded(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobComplete && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func firstLine(s string) string {
	if s == "" {
		return "waiting for quota"
	}
	return strings.SplitN(s, "\n", 2)[0]
}

func joinKeys(m map[string]bool) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ", ")
}

func runOverview(ctx context.Context, c *clients, args []string) error {
	fs := flag.NewFlagSet("overview", flag.ExitOnError)
	watch := fs.Bool("watch", false, "refresh every 2s")
	_ = fs.Parse(args)
	for {
		var b strings.Builder
		if err := overview(ctx, c, &b); err != nil {
			return err
		}
		if *watch {
			clearScreen()
		}
		fmt.Print(b.String())
		if !*watch {
			return nil
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(2 * time.Second):
		}
	}
}

func overview(ctx context.Context, c *clients, b *strings.Builder) error {
	fmt.Fprintf(b, "%s\n\nClusterQueue GPU quota reserved / nominal\n", time.Now().Format("15:04:05"))
	cqs, err := c.dyn.Resource(clusterQueueGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, cq := range cqs.Items {
		used := "0"
		res, _, _ := unstructured.NestedSlice(cq.Object, "status", "flavorsReservation")
		for _, f := range res {
			fm, _ := f.(map[string]any)
			rs, _, _ := unstructured.NestedSlice(fm, "resources")
			for _, r := range rs {
				rm, _ := r.(map[string]any)
				if str(rm["name"]) == "nvidia.com/gpu" {
					used = fmt.Sprint(rm["total"])
				}
			}
		}
		nominal := "?"
		groups, _, _ := unstructured.NestedSlice(cq.Object, "spec", "resourceGroups")
		for _, g := range groups {
			gm, _ := g.(map[string]any)
			fl, _, _ := unstructured.NestedSlice(gm, "flavors")
			for _, f := range fl {
				fm, _ := f.(map[string]any)
				rs, _, _ := unstructured.NestedSlice(fm, "resources")
				for _, r := range rs {
					rm, _ := r.(map[string]any)
					if str(rm["name"]) == "nvidia.com/gpu" {
						nominal = fmt.Sprint(rm["nominalQuota"])
					}
				}
			}
		}
		fmt.Fprintf(b, "  %-16s %s / %s\n", cq.GetName(), used, nominal)
	}

	fmt.Fprintf(b, "\nWorkloads\n")
	wls, err := c.dyn.Resource(workloadGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	if len(wls.Items) == 0 {
		fmt.Fprintf(b, "  (none)\n")
	}
	for _, wl := range wls.Items {
		q, _, _ := unstructured.NestedString(wl.Object, "spec", "queueName")
		state := "Queued"
		switch {
		case isTrue(&wl, "Finished"):
			state = "Finished"
		case isTrue(&wl, "Admitted"):
			state = "Admitted"
		case isTrue(&wl, "QuotaReserved"):
			state = "QuotaReserved"
		}
		fmt.Fprintf(b, "  %-34s queue=%-11s %s\n", wl.GetName(), q, state)
	}

	fmt.Fprintf(b, "\nProvisioningRequests\n")
	prs, err := c.dyn.Resource(provReqGVR).Namespace("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	if len(prs.Items) == 0 {
		fmt.Fprintf(b, "  (none)\n")
	}
	for _, pr := range prs.Items {
		p := conditions(&pr)["Provisioned"]
		fmt.Fprintf(b, "  %-40s Provisioned=%s %s\n", pr.GetName(), orDash(p.Status), p.Reason)
	}

	pool, err := loadPool(ctx, c)
	if err != nil {
		return err
	}
	nodes, err := poolNodes(ctx, c, pool.Name)
	if err != nil {
		return err
	}
	pods, err := c.core.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	perNode := map[string]int{}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if p.Spec.NodeName != "" {
			perNode[p.Spec.NodeName]++
		}
	}
	fmt.Fprintf(b, "\nNode pool %s: %d/%d nodes\n", pool.Name, len(nodes), pool.MaxNodes)
	for _, n := range nodes {
		fmt.Fprintf(b, "  %-24s pods=%d\n", n.Name, perNode[n.Name])
	}
	return nil
}

func clearScreen() {
	if os.Getenv("TAU_DEMO_NO_CLEAR") == "1" {
		fmt.Println("----")
		return
	}
	fmt.Print("\033[H\033[2J")
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
