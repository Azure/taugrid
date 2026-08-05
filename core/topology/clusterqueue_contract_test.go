package topology

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The CLI's expected ClusterQueue names are one half of a contract whose other
// half lives in applications/tau-queues/overlays/*/clusterqueues.yaml, which is
// what ArgoCD actually creates on a cluster. Nothing used to assert the two
// against each other, so two independent de-branding passes renamed one side
// each and picked different names: the CLI expected one value while every
// deployed cluster carried another. Both PRs were green because the tests were
// renamed alongside the constant.
//
// The consequence was not cosmetic. queueresolve refuses to submit when a
// preset's expected ClusterQueue does not match the LocalQueue's actual one, so
// every preset-driven run was hard-blocked on every cluster this repo deploys.
//
// This test fails if a rename touches only one side again.

func overlayRoot(t *testing.T) string {
	t.Helper()
	// core/topology -> repo root
	root, err := filepath.Abs(filepath.Join("..", "..", "applications", "tau-queues", "overlays"))
	if err != nil {
		t.Fatalf("resolve overlay root: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Skipf("overlay assets not present at %s (%v); contract cannot be checked from this tree", root, err)
	}
	return root
}

// clusterQueueNamesInOverlays returns every `name:` declared directly under a
// `kind: ClusterQueue` document in the GitOps overlays.
func clusterQueueNamesInOverlays(t *testing.T, root string) map[string][]string {
	t.Helper()
	found := map[string][]string{}
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || filepath.Base(path) != "clusterqueues.yaml" {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for _, doc := range strings.Split(string(raw), "\n---") {
			if !strings.Contains(doc, "kind: ClusterQueue") {
				continue
			}
			for _, line := range strings.Split(doc, "\n") {
				trimmed := strings.TrimSpace(line)
				if !strings.HasPrefix(trimmed, "name:") {
					continue
				}
				// Only metadata.name is indented exactly two spaces.
				if !strings.HasPrefix(line, "  name:") || strings.HasPrefix(line, "   ") {
					continue
				}
				name := strings.TrimSpace(strings.TrimPrefix(trimmed, "name:"))
				name = strings.Trim(name, `"'`)
				if name != "" {
					found[name] = append(found[name], path)
				}
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk overlays: %v", err)
	}
	return found
}

func TestSharedClusterQueueNamesExistInGitOpsOverlays(t *testing.T) {
	root := overlayRoot(t)
	declared := clusterQueueNamesInOverlays(t, root)

	// Positive control: the parser must actually find ClusterQueues. Without
	// this, a parser that silently returns nothing would make every assertion
	// below vacuously pass -- the exact failure mode this file exists to catch.
	if len(declared) == 0 {
		t.Fatalf("parsed zero ClusterQueue names from %s; the parser, not the contract, is broken", root)
	}

	for _, want := range []string{SharedGPUClusterQueueName, sharedDRAClusterQueueName} {
		if _, ok := declared[want]; !ok {
			names := make([]string, 0, len(declared))
			for n := range declared {
				names = append(names, n)
			}
			t.Errorf("topology expects ClusterQueue %q but no GitOps overlay declares it; overlays declare %v.\n"+
				"A preset naming a ClusterQueue that no cluster creates makes queueresolve reject every preset-driven run.\n"+
				"Fix whichever side is wrong -- do not rename only one.", want, names)
		}
	}
}

func TestTopologyAssetClusterQueuesMatchConstants(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("assets", "azure-topology-policy.yaml"))
	if err != nil {
		t.Fatalf("read topology asset: %v", err)
	}

	var assetQueues []string
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "clusterQueue:") {
			continue
		}
		name := strings.TrimSpace(strings.TrimPrefix(trimmed, "clusterQueue:"))
		assetQueues = append(assetQueues, strings.Trim(name, `"'`))
	}

	// Positive control: the asset does declare clusterQueue lines. If this ever
	// returns zero the assertion below proves nothing.
	if len(assetQueues) == 0 {
		t.Fatalf("found no clusterQueue: lines in the topology asset; the parser is broken, not the asset")
	}

	allowed := map[string]bool{
		SharedGPUClusterQueueName: true,
		sharedDRAClusterQueueName: true,
	}
	for _, q := range assetQueues {
		if !allowed[q] {
			t.Errorf("topology asset names ClusterQueue %q, which matches neither SharedGPUClusterQueueName (%q) nor sharedDRAClusterQueueName (%q)",
				q, SharedGPUClusterQueueName, sharedDRAClusterQueueName)
		}
	}
}
