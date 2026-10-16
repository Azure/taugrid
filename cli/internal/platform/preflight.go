// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package platform

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Azure/taugrid/cli/internal/depcontract"
)

type Runner interface {
	Raw(ctx context.Context, args []string, stdin []byte) (string, error)
}

type Worker struct {
	Context string
	Runner  Runner
}

type CheckStatus string

const (
	StatusOK   CheckStatus = "ok"
	StatusFail CheckStatus = "fail"
	StatusInfo CheckStatus = "info"
)

const KindStorageClass depcontract.Kind = "StorageClass"

type Result struct {
	Kind      depcontract.Kind
	Namespace string
	Name      string
	Context   string
	Status    CheckStatus
	Message   string
}

type Report struct {
	Results []Result
}

func (r Report) Failures() []Result {
	var out []Result
	for _, res := range r.Results {
		if res.Status == StatusFail {
			out = append(out, res)
		}
	}
	return out
}

func (r Report) OK() bool {
	return len(r.Failures()) == 0
}

func (r Report) Summary() string {
	if len(r.Results) == 0 {
		return "platform preflight: no checkable dependencies found"
	}
	var b strings.Builder
	for _, res := range r.Results {
		ctx := res.Context
		if ctx == "" {
			ctx = "(manifest)"
		}
		fmt.Fprintf(&b, "[%-4s] %-19s %-30s worker=%s", strings.ToUpper(string(res.Status)), string(res.Kind), qualifiedName(res.Namespace, res.Name), ctx)
		if res.Message != "" {
			fmt.Fprintf(&b, " %s", res.Message)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func (r Report) Err() error {
	fails := r.Failures()
	if len(fails) == 0 {
		return nil
	}
	order := []string{}
	byDep := map[string][]string{}
	for _, f := range fails {
		key := fmt.Sprintf("%s %s", f.Kind, qualifiedName(f.Namespace, f.Name))
		if _, ok := byDep[key]; !ok {
			order = append(order, key)
		}
		ctx := f.Context
		if ctx == "" {
			ctx = "manifest"
		}
		byDep[key] = append(byDep[key], fmt.Sprintf("%s: %s", ctx, f.Message))
	}
	lines := make([]string, 0, len(order))
	for _, key := range order {
		lines = append(lines, fmt.Sprintf("%s failed on %s", key, strings.Join(byDep[key], "; ")))
	}
	return fmt.Errorf("platform preflight failed:\n  %s", strings.Join(lines, "\n  "))
}

type Options struct {
	NamespaceOverride string
}

func namespacedDependencyKind(kind depcontract.Kind) bool {
	switch kind {
	case depcontract.KindSecret,
		depcontract.KindImagePullSecret,
		depcontract.KindServiceAccount,
		depcontract.KindSecretProviderClass,
		depcontract.KindPersistentVolumeClaim,
		depcontract.KindUnsupported:
		return true
	default:
		return false
	}
}

func namespacedKubectlDependencyKind(kind depcontract.Kind) bool {
	switch kind {
	case depcontract.KindSecret,
		depcontract.KindImagePullSecret,
		depcontract.KindServiceAccount,
		depcontract.KindSecretProviderClass,
		depcontract.KindPersistentVolumeClaim:
		return true
	default:
		return false
	}
}

func effectiveNamespace(dep depcontract.Dependency, opts Options) string {
	ns := dep.Namespace
	if opts.NamespaceOverride != "" && namespacedDependencyKind(dep.Kind) {
		return opts.NamespaceOverride
	}
	return ns
}

func CheckMultiKueuePreflight(ctx context.Context, workers []Worker, deps []depcontract.Dependency, opts Options) (Report, error) {
	if len(workers) == 0 {
		return Report{}, fmt.Errorf("platform preflight: at least one --worker-context is required")
	}

	var report Report
	for _, dep := range deps {
		ns := effectiveNamespace(dep, opts)
		if !namespacedKubectlDependencyKind(dep.Kind) || ns != "" {
			continue
		}
		report.Results = append(report.Results, Result{
			Kind:   dep.Kind,
			Name:   dep.Name,
			Status: StatusFail,
			Message: "namespaced dependency has empty namespace after applying any override; " +
				"set metadata.namespace in the manifest or pass --namespace",
		})
	}
	if !report.OK() {
		return report, nil
	}

	pvcStorageClasses := map[pvcRef]map[string]string{}

	for _, dep := range deps {
		ns := effectiveNamespace(dep, opts)

		switch dep.Kind {
		case depcontract.KindImage:
			report.Results = append(report.Results, Result{
				Kind:   dep.Kind,
				Name:   dep.Name,
				Status: StatusInfo,
				Message: "image pull availability cannot be proven by kubectl get; " +
					"this is inventory only — see issue #871 for live-pull canary evidence",
			})

		case depcontract.KindUnsupported:
			report.Results = append(report.Results, Result{
				Kind:      dep.Kind,
				Namespace: ns,
				Name:      dep.Name,
				Status:    StatusFail,
				Message:   fmt.Sprintf("unsupported dependency: %s", dep.Detail),
			})

		case depcontract.KindSecret, depcontract.KindImagePullSecret:
			if dep.IsRedactedPlaceholder() {
				report.Results = append(report.Results, Result{
					Kind:      dep.Kind,
					Namespace: ns,
					Name:      dep.Name,
					Status:    StatusFail,
					Message:   "manifest was rendered with client dry-run redaction (--dry-run=client); re-render without redaction before running preflight",
				})
				continue
			}
			checkNamedObject(ctx, workers, "secret", ns, dep.Name, dep.Kind, &report)

		case depcontract.KindServiceAccount:
			checkNamedObject(ctx, workers, "serviceaccount", ns, dep.Name, dep.Kind, &report)

		case depcontract.KindSecretProviderClass:
			checkNamedObject(ctx, workers, "secretproviderclasses.secrets-store.csi.x-k8s.io", ns, dep.Name, dep.Kind, &report)

		case depcontract.KindPersistentVolumeClaim:
			ref := pvcRef{Namespace: ns, Name: dep.Name}
			pvcStorageClasses[ref] = checkPVC(ctx, workers, ref.Namespace, ref.Name, &report)

		default:
			report.Results = append(report.Results, Result{
				Kind:      dep.Kind,
				Namespace: ns,
				Name:      dep.Name,
				Status:    StatusFail,
				Message:   fmt.Sprintf("platform preflight does not know how to check dependency kind %q", dep.Kind),
			})
		}
	}

	checkStorageClassParity(ctx, workers, pvcStorageClasses, &report)

	return report, nil
}

func checkNamedObject(ctx context.Context, workers []Worker, resource, namespace, name string, kind depcontract.Kind, report *Report) {
	for _, w := range workers {
		args := []string{"get", resource, name, "-o", "name"}
		if namespace != "" {
			args = append([]string{"-n", namespace}, args...)
		}
		_, err := w.Runner.Raw(ctx, args, nil)
		result := Result{
			Kind:      kind,
			Namespace: namespace,
			Name:      name,
			Context:   w.Context,
			Status:    StatusOK,
		}
		if err != nil {
			result.Status = StatusFail
			result.Message = fmt.Sprintf("not found: %v", err)
		}
		report.Results = append(report.Results, result)
	}
}

type pvcSpecDoc struct {
	Spec struct {
		StorageClassName string `json:"storageClassName"`
	} `json:"spec"`
	Status *struct {
		Phase *string `json:"phase"`
	} `json:"status,omitempty"`
}

type pvcRef struct {
	Namespace string
	Name      string
}

func (r pvcRef) QualifiedName() string {
	return qualifiedName(r.Namespace, r.Name)
}

func pvcObservedPhase(doc pvcSpecDoc) string {
	if doc.Status == nil || doc.Status.Phase == nil {
		return "<missing>"
	}
	phase := strings.TrimSpace(*doc.Status.Phase)
	if phase == "" {
		return "<empty>"
	}
	return phase
}

func checkPVC(ctx context.Context, workers []Worker, namespace, name string, report *Report) map[string]string {
	classes := map[string]string{}
	for _, w := range workers {
		args := []string{"get", "pvc", name, "-o", "json"}
		if namespace != "" {
			args = append([]string{"-n", namespace}, args...)
		}
		raw, err := w.Runner.Raw(ctx, args, nil)
		if err != nil {
			report.Results = append(report.Results, Result{
				Kind: depcontract.KindPersistentVolumeClaim, Namespace: namespace, Name: name, Context: w.Context, Status: StatusFail,
				Message: fmt.Sprintf("platform-managed PVC not found; pre-provision and bind it on this worker before dispatch: %v", err),
			})
			continue
		}
		var doc pvcSpecDoc
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			report.Results = append(report.Results, Result{
				Kind: depcontract.KindPersistentVolumeClaim, Namespace: namespace, Name: name, Context: w.Context, Status: StatusFail,
				Message: fmt.Sprintf("failed to parse PVC json: %v", err),
			})
			continue
		}
		classes[w.Context] = doc.Spec.StorageClassName
		phase := pvcObservedPhase(doc)
		if phase != "Bound" {
			report.Results = append(report.Results, Result{
				Kind: depcontract.KindPersistentVolumeClaim, Namespace: namespace, Name: name, Context: w.Context, Status: StatusFail,
				Message: fmt.Sprintf("platform-managed PVC exists but is not Bound (phase=%s, storageClass=%q); wait for the platform storage lifecycle", phase, doc.Spec.StorageClassName),
			})
			continue
		}
		report.Results = append(report.Results, Result{
			Kind: depcontract.KindPersistentVolumeClaim, Namespace: namespace, Name: name, Context: w.Context, Status: StatusOK,
		})
	}
	return classes
}

func checkStorageClassParity(ctx context.Context, workers []Worker, pvcStorageClasses map[pvcRef]map[string]string, report *Report) {
	seenClasses := map[string]bool{}

	pvcRefs := make([]pvcRef, 0, len(pvcStorageClasses))
	for ref := range pvcStorageClasses {
		pvcRefs = append(pvcRefs, ref)
	}
	sort.Slice(pvcRefs, func(i, j int) bool {
		if pvcRefs[i].Namespace != pvcRefs[j].Namespace {
			return pvcRefs[i].Namespace < pvcRefs[j].Namespace
		}
		return pvcRefs[i].Name < pvcRefs[j].Name
	})

	for _, ref := range pvcRefs {
		byContext := pvcStorageClasses[ref]
		var first string
		haveFirst := false
		mismatched := false
		for _, w := range workers {
			cls, ok := byContext[w.Context]
			if !ok {
				continue
			}
			seenClasses[cls] = true
			if !haveFirst {
				first, haveFirst = cls, true
				continue
			}
			if cls != first {
				mismatched = true
			}
		}
		if !mismatched {
			continue
		}
		details := make([]string, 0, len(byContext))
		for _, w := range workers {
			if cls, ok := byContext[w.Context]; ok {
				details = append(details, fmt.Sprintf("%s=%q", w.Context, cls))
			}
		}
		report.Results = append(report.Results, Result{
			Kind:      KindStorageClass,
			Namespace: ref.Namespace,
			Name:      ref.Name,
			Status:    StatusFail,
			Message:   fmt.Sprintf("PVC %q resolves to different StorageClass names across workers: %s", ref.QualifiedName(), strings.Join(details, ", ")),
		})
	}

	classNames := make([]string, 0, len(seenClasses))
	for cls := range seenClasses {
		if cls != "" {
			classNames = append(classNames, cls)
		}
	}
	sort.Strings(classNames)
	for _, cls := range classNames {
		for _, w := range workers {
			_, err := w.Runner.Raw(ctx, []string{"get", "storageclass", cls, "-o", "name"}, nil)
			if err != nil {
				report.Results = append(report.Results, Result{
					Kind: KindStorageClass, Name: cls, Context: w.Context, Status: StatusFail,
					Message: fmt.Sprintf("not found: %v", err),
				})
				continue
			}
			report.Results = append(report.Results, Result{
				Kind: KindStorageClass, Name: cls, Context: w.Context, Status: StatusOK,
			})
		}
	}
}

func qualifiedName(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "/" + name
}
