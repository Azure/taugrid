package topology

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The CLI's expected ClusterQueue names must match any platform-owned GitOps
// overlays supplied alongside this repository. The test is skipped when those
// optional deployment assets are not present.

func overlayRoot(t *testing.T) string {
	t.Helper()
	// core/topology -> repo root
	root, err := filepath.Abs(filepath.Join("..", "..", "deploy", "queues"))
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

	if len(declared) == 0 {
		t.Skipf("no platform ClusterQueue overlays present under %s", root)
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
