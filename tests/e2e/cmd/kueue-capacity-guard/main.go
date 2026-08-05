package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/api/resource"
)

const capacityInventoryKind = "KueueCapacityInventory"

type typeMeta struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
}

type metadata struct {
	Name string `yaml:"name"`
}

type capacityInventory struct {
	typeMeta `yaml:",inline"`
	Metadata metadata `yaml:"metadata"`
	Spec     struct {
		Resources []capacityEntry `yaml:"resources"`
	} `yaml:"spec"`
}

type capacityEntry struct {
	Flavor   string `yaml:"flavor"`
	Resource string `yaml:"resource"`
	Capacity string `yaml:"capacity"`
	Reserve  string `yaml:"reserve"`
}

type clusterQueueManifest struct {
	typeMeta `yaml:",inline"`
	Metadata metadata         `yaml:"metadata"`
	Spec     clusterQueueSpec `yaml:"spec"`
}

type clusterQueueSpec struct {
	Cohort         string          `yaml:"cohort"`
	CohortName     string          `yaml:"cohortName"`
	ResourceGroups []resourceGroup `yaml:"resourceGroups"`
}

type resourceGroup struct {
	Flavors []flavorQuota `yaml:"flavors"`
}

type flavorQuota struct {
	Name      string          `yaml:"name"`
	Resources []resourceQuota `yaml:"resources"`
}

type resourceQuota struct {
	Name         string `yaml:"name"`
	NominalQuota string `yaml:"nominalQuota"`
}

type capacityKey struct {
	Flavor   string
	Resource string
}

type capacityLimit struct {
	Capacity  resource.Quantity
	Reserve   resource.Quantity
	Available resource.Quantity
	Source    string
}

type quotaRecord struct {
	ClusterQueue string
	Flavor       string
	Resource     string
	Quantity     resource.Quantity
	Source       string
}

type quotaLoadResult struct {
	Records       []quotaRecord
	ClusterQueues map[string]struct{}
}

type validationReport struct {
	ClusterQueues int
	Totals        map[capacityKey]resource.Quantity
}

type validationError []string

func (e validationError) Error() string {
	return strings.Join(e, "\n")
}

func main() {
	capacityPath := flag.String("capacity", "", "path to a KueueCapacityInventory YAML file")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s --capacity capacity.yaml <clusterqueue.yaml|dir>...\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()

	if *capacityPath == "" || flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}

	limits, err := loadCapacityInventory(*capacityPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading capacity inventory: %v\n", err)
		os.Exit(1)
	}
	quotas, err := loadClusterQueueQuotas(flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "loading ClusterQueues: %v\n", err)
		os.Exit(1)
	}
	report, err := validateCapacity(limits, quotas)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Kueue quota/capacity validation failed:\n%v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Kueue quota/capacity validation passed: %d ClusterQueues, %d quota entries\n",
		report.ClusterQueues, len(quotas.Records))
	for _, key := range sortedCapacityKeys(report.Totals) {
		limit := limits[key]
		total := report.Totals[key]
		fmt.Printf("- flavor=%s resource=%s nominal=%s available=%s capacity=%s reserve=%s\n",
			key.Flavor, key.Resource, total.String(), limit.Available.String(), limit.Capacity.String(), limit.Reserve.String())
	}
}

func loadCapacityInventory(path string) (map[capacityKey]capacityLimit, error) {
	limits := map[capacityKey]capacityLimit{}
	inventories := 0
	err := decodeYAMLDocuments(path, func(node *yaml.Node) error {
		var meta typeMeta
		if err := node.Decode(&meta); err != nil {
			return fmt.Errorf("%s: decoding type metadata: %w", path, err)
		}
		if meta.Kind == "" {
			return nil
		}
		if meta.Kind != capacityInventoryKind {
			return nil
		}
		inventories++

		var inventory capacityInventory
		if err := node.Decode(&inventory); err != nil {
			return fmt.Errorf("%s: decoding %s: %w", path, capacityInventoryKind, err)
		}
		for i, entry := range inventory.Spec.Resources {
			key := capacityKey{Flavor: strings.TrimSpace(entry.Flavor), Resource: strings.TrimSpace(entry.Resource)}
			if key.Flavor == "" || key.Resource == "" {
				return fmt.Errorf("%s: spec.resources[%d] requires flavor and resource", path, i)
			}
			if _, exists := limits[key]; exists {
				return fmt.Errorf("%s: duplicate capacity for flavor=%q resource=%q", path, key.Flavor, key.Resource)
			}

			capacity, err := parseQuantity(entry.Capacity, fmt.Sprintf("%s: spec.resources[%d].capacity", path, i))
			if err != nil {
				return err
			}
			reserve := resource.MustParse("0")
			if strings.TrimSpace(entry.Reserve) != "" {
				reserve, err = parseQuantity(entry.Reserve, fmt.Sprintf("%s: spec.resources[%d].reserve", path, i))
				if err != nil {
					return err
				}
			}
			if reserve.Cmp(capacity) > 0 {
				return fmt.Errorf("%s: spec.resources[%d].reserve %s exceeds capacity %s", path, i, reserve.String(), capacity.String())
			}
			available := capacity.DeepCopy()
			available.Sub(reserve)
			limits[key] = capacityLimit{
				Capacity:  capacity,
				Reserve:   reserve,
				Available: available,
				Source:    path,
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if inventories == 0 {
		return nil, fmt.Errorf("%s: no %s document found", path, capacityInventoryKind)
	}
	if len(limits) == 0 {
		return nil, fmt.Errorf("%s: %s has no spec.resources entries", path, capacityInventoryKind)
	}
	return limits, nil
}

func loadClusterQueueQuotas(inputs []string) (quotaLoadResult, error) {
	files, err := expandInputs(inputs)
	if err != nil {
		return quotaLoadResult{}, err
	}

	result := quotaLoadResult{ClusterQueues: map[string]struct{}{}}
	for _, path := range files {
		err := decodeYAMLDocuments(path, func(node *yaml.Node) error {
			var meta typeMeta
			if err := node.Decode(&meta); err != nil {
				return fmt.Errorf("%s: decoding type metadata: %w", path, err)
			}
			if meta.Kind != "ClusterQueue" {
				return nil
			}

			var cq clusterQueueManifest
			if err := node.Decode(&cq); err != nil {
				return fmt.Errorf("%s: decoding ClusterQueue: %w", path, err)
			}
			cqName := strings.TrimSpace(cq.Metadata.Name)
			if cqName == "" {
				return fmt.Errorf("%s: ClusterQueue metadata.name is required", path)
			}
			result.ClusterQueues[cqName] = struct{}{}
			for groupIndex, group := range cq.Spec.ResourceGroups {
				for flavorIndex, flavor := range group.Flavors {
					flavorName := strings.TrimSpace(flavor.Name)
					if flavorName == "" {
						return fmt.Errorf("%s: ClusterQueue %s resourceGroups[%d].flavors[%d].name is required",
							path, cqName, groupIndex, flavorIndex)
					}
					for resourceIndex, rq := range flavor.Resources {
						resourceName := strings.TrimSpace(rq.Name)
						if resourceName == "" {
							return fmt.Errorf("%s: ClusterQueue %s resourceGroups[%d].flavors[%d].resources[%d].name is required",
								path, cqName, groupIndex, flavorIndex, resourceIndex)
						}
						quantity, err := parseQuantity(rq.NominalQuota,
							fmt.Sprintf("%s: ClusterQueue %s flavor=%s resource=%s nominalQuota",
								path, cqName, flavorName, resourceName))
						if err != nil {
							return err
						}
						result.Records = append(result.Records, quotaRecord{
							ClusterQueue: cqName,
							Flavor:       flavorName,
							Resource:     resourceName,
							Quantity:     quantity,
							Source:       path,
						})
					}
				}
			}
			return nil
		})
		if err != nil {
			return quotaLoadResult{}, err
		}
	}
	if len(result.ClusterQueues) == 0 {
		return quotaLoadResult{}, fmt.Errorf("no ClusterQueue manifests found in inputs: %s", strings.Join(inputs, ", "))
	}
	return result, nil
}

func validateCapacity(limits map[capacityKey]capacityLimit, quotas quotaLoadResult) (validationReport, error) {
	totals := map[capacityKey]resource.Quantity{}
	var problems validationError

	for _, quota := range quotas.Records {
		key := capacityKey{Flavor: quota.Flavor, Resource: quota.Resource}
		if _, ok := limits[key]; !ok {
			problems = append(problems, fmt.Sprintf("%s: ClusterQueue %s declares nominalQuota %s for flavor=%s resource=%s but capacity inventory has no matching entry",
				quota.Source, quota.ClusterQueue, quota.Quantity.String(), key.Flavor, key.Resource))
			continue
		}
		total := totals[key]
		total.Add(quota.Quantity)
		totals[key] = total
	}

	for _, key := range sortedCapacityKeys(totals) {
		limit := limits[key]
		total := totals[key]
		if total.Cmp(limit.Available) > 0 {
			problems = append(problems, fmt.Sprintf("flavor=%s resource=%s reserved nominalQuota %s exceeds available capacity %s (capacity %s - reserve %s)",
				key.Flavor, key.Resource, total.String(), limit.Available.String(), limit.Capacity.String(), limit.Reserve.String()))
		}
	}

	report := validationReport{
		ClusterQueues: len(quotas.ClusterQueues),
		Totals:        totals,
	}
	if len(problems) > 0 {
		return report, problems
	}
	return report, nil
}

func parseQuantity(raw, field string) (resource.Quantity, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return resource.Quantity{}, fmt.Errorf("%s is required", field)
	}
	quantity, err := resource.ParseQuantity(value)
	if err != nil {
		return resource.Quantity{}, fmt.Errorf("%s=%q is not a valid Kubernetes quantity: %w", field, value, err)
	}
	return quantity, nil
}

func decodeYAMLDocuments(path string, handle func(*yaml.Node) error) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	for {
		var node yaml.Node
		err := decoder.Decode(&node)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if node.Kind == 0 || len(node.Content) == 0 {
			continue
		}
		if err := handle(&node); err != nil {
			return err
		}
	}
	return nil
}

func expandInputs(inputs []string) ([]string, error) {
	var files []string
	for _, input := range inputs {
		info, err := os.Stat(input)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, input)
			continue
		}

		err = filepath.WalkDir(input, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			if hasYAMLSuffix(path) {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	if len(files) == 0 {
		return nil, fmt.Errorf("no YAML files found in inputs: %s", strings.Join(inputs, ", "))
	}
	return files, nil
}

func hasYAMLSuffix(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}

func sortedCapacityKeys(values map[capacityKey]resource.Quantity) []capacityKey {
	keys := make([]capacityKey, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Flavor != keys[j].Flavor {
			return keys[i].Flavor < keys[j].Flavor
		}
		return keys[i].Resource < keys[j].Resource
	})
	return keys
}
