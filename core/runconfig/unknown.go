package runconfig

import (
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Unknown-key handling for SDK-generated managed workflow manifests.
//
// Direct run configs decode with yaml KnownFields(true), so a typo is already a
// hard error. Managed manifests cannot: parse() decodes them into
// managedConfigProjection, which is deliberately partial. Keys like
// resource_naming, runtime.env and compute.cpus belong to the manifest schema in
// cli/internal/manifest and are consumed there, not here. Strict decoding
// against a partial view would reject valid manifests wholesale.
//
// So managed manifests get a warning instead. That still closes the real hole:
// a misspelled scheduling directive (policy.node_selector written as
// scheduling.node_selector) used to be dropped in silence, and the workload
// would schedule anywhere.

// managedPassthroughPaths are keys owned by the managed workflow manifest schema
// rather than by Config. They are legitimate in a managed manifest, so they must
// not warn, but runconfig only projects a subset of them.
//
// The value is whether the key is a structured field whose children this package
// also models. True means "descend": a typo underneath it still warns. False
// means the subtree is opaque -- either a scalar, or a container whose contents
// are user data rather than schema.
//
// Structured entries matter because these subtrees carry directives. A silently
// dropped runtime.rdma.enabled leaves an IB/NCCL job without its RDMA resource,
// and a dropped storage.mounts[].readOnly renders a PVC writable -- the same
// fail-open this warning exists to close, one level down.
//
// TestManagedPassthroughCoversManifestSchema in cli/internal/manifest reflects
// over the real manifest.Manifest type and fails if this list drifts.
var managedPassthroughPaths = map[string]bool{
	"eval":                        false,
	"artifacts":                   false,
	"research":                    false,
	"model":                       false,
	"resource_naming":             false,
	"compute.cpus":                false,
	"compute.cpu_limit":           false,
	"compute.memory":              false,
	"compute.memory_limit":        false,
	"compute.worker_cpus":         false,
	"compute.worker_cpu_limit":    false,
	"compute.worker_memory":       false,
	"compute.worker_memory_limit": false,
	"runtime.rdma":                true,
	"runtime.rdma.enabled":        false,
	"runtime.rdma.resource_name":  false,
	"runtime.rdma.count":          false,
	"storage.mounts":              true,
	"storage.mounts.name":         false,
	"storage.mounts.mountPath":    false,
	"storage.mounts.pvc":          false,
	"storage.mounts.readOnly":     false,
}

// documentedPassthroughPaths are keys that belong to neither schema and are
// tolerated anyway. It is deliberately empty: provenance and commentary belong
// in YAML comments, which have the virtue of not looking like configuration
// that does something. Adding an entry here should require justifying why a
// key must look live but do nothing.
var documentedPassthroughPaths = map[string]bool{}

// configSchemaPaths reports every path Config models, mapping each to whether it
// is a nested struct worth descending into. Free-form maps (policy.node_selector,
// runtime.env_secret, execution.param_space, configs) are leaves: their keys are
// user data, not schema, and must never be validated as field names.
func configSchemaPaths() map[string]bool {
	out := map[string]bool{}
	for _, path := range configFieldPaths() {
		out[path] = false
	}
	for _, path := range configStructPaths() {
		out[path] = true
	}
	return out
}

// IsKnownManagedKey reports whether a dotted path in a managed workflow manifest
// is recognized, either because Config models it or because it is an accepted
// pass-through. Exported so cli/internal/manifest can assert, by reflection over
// the real manifest type, that this package never warns about a legitimate
// manifest field.
// ManagedPassthroughPaths returns the manifest-owned keys this package tolerates
// without warning. Exported so cli/internal/manifest can assert the list in both
// directions: that no manifest field is missing from it (a missing entry warns
// spuriously) and that no entry has outlived the field it was added for (a stale
// entry silently permits a key that no longer exists).
func ManagedPassthroughPaths() []string {
	out := make([]string, 0, len(managedPassthroughPaths))
	for path := range managedPassthroughPaths {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}

func IsKnownManagedKey(path string) bool {
	if _, ok := managedPassthroughPaths[path]; ok {
		return true
	}
	if documentedPassthroughPaths[path] {
		return true
	}
	// A key nested under a pass-through parent is covered by that parent, but
	// only when that parent is opaque. A structured pass-through models its own
	// children, so an unlisted child under it is a typo, not user data.
	for prefix := path; ; {
		idx := strings.LastIndex(prefix, ".")
		if idx < 0 {
			break
		}
		prefix = prefix[:idx]
		if descend, ok := managedPassthroughPaths[prefix]; ok {
			return !descend
		}
		if documentedPassthroughPaths[prefix] {
			return true
		}
		if descend, ok := configSchemaPaths()[prefix]; ok && !descend {
			// Parent is a leaf in Config (a free-form map or scalar), so its
			// children are user data and are never validated.
			return true
		}
	}
	_, ok := configSchemaPaths()[path]
	return ok
}

// UnknownKey is one unrecognized key plus the child keys nested under it, which
// are used to infer what the author meant when the key is a container.
type UnknownKey struct {
	Path     string
	Children []string
}

// UnknownKeys returns the dotted paths in a managed workflow manifest that
// neither Config nor the managed manifest schema recognizes.
func UnknownKeys(raw []byte) ([]UnknownKey, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 {
		return nil, nil
	}
	known := configSchemaPaths()
	var unknown []UnknownKey
	collectUnknownNode(doc.Content[0], "", known, &unknown)
	unknown = dedupeUnknown(unknown)
	sort.Slice(unknown, func(i, j int) bool { return unknown[i].Path < unknown[j].Path })
	return unknown, nil
}

// dedupeUnknown collapses the same path reported from several elements of a
// list: a typo repeated across three storage.mounts entries is one mistake.
func dedupeUnknown(in []UnknownKey) []UnknownKey {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, key := range in {
		if seen[key.Path] {
			continue
		}
		seen[key.Path] = true
		out = append(out, key)
	}
	return out
}

// collectUnknownNode walks a mapping, or every element of a list of mappings.
// List elements share their parent's path, so a typo in one storage.mounts entry
// reports as storage.mounts.readOlny rather than carrying an index the schema
// has no name for.
func collectUnknownNode(node *yaml.Node, prefix string, known map[string]bool, out *[]UnknownKey) {
	if node == nil {
		return
	}
	switch node.Kind {
	case yaml.MappingNode:
		collectUnknown(node, prefix, known, out)
	case yaml.SequenceNode:
		for _, item := range node.Content {
			collectUnknownNode(item, prefix, known, out)
		}
	}
}

func collectUnknown(node *yaml.Node, prefix string, known map[string]bool, out *[]UnknownKey) {
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key := node.Content[i].Value
		if key == "" {
			continue
		}
		path := key
		if prefix != "" {
			path = prefix + "." + key
		}
		if descend, ok := managedPassthroughPaths[path]; ok {
			if descend {
				collectUnknownNode(node.Content[i+1], path, known, out)
			}
			continue
		}
		if documentedPassthroughPaths[path] {
			continue
		}
		descend, ok := known[path]
		if !ok {
			*out = append(*out, UnknownKey{Path: path, Children: childKeys(node.Content[i+1])})
			continue
		}
		if descend {
			collectUnknownNode(node.Content[i+1], path, known, out)
		}
	}
}

func childKeys(node *yaml.Node) []string {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	keys := make([]string, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		if key := node.Content[i].Value; key != "" {
			keys = append(keys, key)
		}
	}
	return keys
}

// describeUnknownKey renders one warning, naming the offending key and pointing
// at the nearest real field when there is an unambiguous candidate.
func describeUnknownKey(key UnknownKey, source string) string {
	msg := fmt.Sprintf("warning: unknown config key %q in %s (ignored)", key.Path, source)
	if suggestion, ok := suggestForKey(key); ok {
		msg += fmt.Sprintf("; did you mean %q?", suggestion)
	}
	return msg
}

// suggestForKey prefers edit distance on the key itself, then falls back to
// inferring the intended parent from the children. The fallback is what catches
// the mistake this warning exists for: `scheduling: {node_selector: ...}` is
// nowhere near "policy" as a string, but its child names it exactly.
func suggestForKey(key UnknownKey) (string, bool) {
	if suggestion, ok := suggestFieldPath(key.Path); ok {
		return suggestion, true
	}
	return suggestParentFromChildren(key)
}

func suggestParentFromChildren(key UnknownKey) (string, bool) {
	if len(key.Children) == 0 {
		return "", false
	}
	parents := map[string]int{}
	for _, child := range key.Children {
		suggestion, ok := suggestFieldPath(key.Path + "." + child)
		if !ok {
			continue
		}
		idx := strings.LastIndex(suggestion, ".")
		if idx < 0 {
			continue
		}
		parents[suggestion[:idx]]++
	}
	// Only speak up when every child agrees; a split vote is a guess.
	if len(parents) != 1 {
		return "", false
	}
	for parent, count := range parents {
		if count == len(key.Children) {
			return parent, true
		}
	}
	return "", false
}

// suggestFieldPath finds the closest known field path by edit distance. It also
// compares leaf names, but only between paths of equal depth: that catches a
// wrong parent with a correct leaf (scheduling.node_selector ->
// policy.node_selector) without ever suggesting a top-level field for a nested
// typo.
func suggestFieldPath(path string) (string, bool) {
	candidates := configFieldPaths()
	leaf, depth := leafAndDepth(path)

	best := ""
	bestDistance := -1
	bestPathDistance := -1
	for _, candidate := range candidates {
		candidateLeaf, candidateDepth := leafAndDepth(candidate)
		pathDistance := levenshtein(path, candidate)
		distance := pathDistance
		if candidateDepth == depth {
			if leafDistance := levenshtein(leaf, candidateLeaf); leafDistance < distance {
				distance = leafDistance
			}
		}
		// Break leaf-distance ties on the full path, so runtime.imag prefers
		// runtime.image over the equally-close-by-leaf run.image.
		if bestDistance < 0 || distance < bestDistance ||
			(distance == bestDistance && pathDistance < bestPathDistance) {
			best, bestDistance, bestPathDistance = candidate, distance, pathDistance
		}
	}
	// Require the match to be closer than "a third of the name is wrong",
	// so genuinely unrelated keys get no misleading suggestion.
	if bestDistance < 0 || bestDistance > maxSuggestionDistance(leaf) {
		return "", false
	}
	return best, true
}

func leafAndDepth(path string) (string, int) {
	depth := strings.Count(path, ".")
	if idx := strings.LastIndex(path, "."); idx >= 0 {
		return path[idx+1:], depth
	}
	return path, depth
}

func maxSuggestionDistance(leaf string) int {
	limit := len(leaf) / 3
	if limit < 2 {
		limit = 2
	}
	if limit > 6 {
		limit = 6
	}
	return limit
}

func levenshtein(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min3(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
