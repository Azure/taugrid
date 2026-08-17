package cli

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

const maxRunPhysicalNameLen = 47

type runIdentity struct {
	LogicalName  string
	RunID        string
	PhysicalName string
}

func ensureRunIdentity(options *runDispatchOptions, logicalName string) (runIdentity, error) {
	logicalName = strings.TrimSpace(logicalName)
	if logicalName == "" {
		return runIdentity{}, fmt.Errorf("run identity requires a logical name")
	}
	if problems := validation.IsDNS1123Label(logicalName); len(problems) > 0 {
		return runIdentity{}, fmt.Errorf("run name %q is invalid: %s", logicalName, strings.Join(problems, "; "))
	}
	if options.logicalName != "" && options.logicalName != logicalName {
		return runIdentity{}, fmt.Errorf("run identity already belongs to logical name %q, not %q", options.logicalName, logicalName)
	}
	if options.runID == "" {
		if options.dryRun == "client" {
			// Client renders remain deterministic and intentionally show the
			// logical name. Server dry-runs and real submissions use immutable
			// execution-qualified names because they reach the API server.
			options.runID = logicalName
		} else {
			runID, err := newMetricsSessionID()
			if err != nil {
				return runIdentity{}, fmt.Errorf("generate run ID: %w", err)
			}
			options.runID = runID
		}
	}
	if options.physicalName == "" {
		if options.dryRun == "client" {
			options.physicalName = logicalName
		} else {
			options.physicalName = physicalRunName(logicalName, options.runID)
		}
	}
	options.logicalName = logicalName
	return runIdentity{
		LogicalName:  options.logicalName,
		RunID:        options.runID,
		PhysicalName: options.physicalName,
	}, nil
}

func physicalRunName(logicalName, runID string) string {
	suffix := strings.Trim(strings.ToLower(runID), "-")
	nameBudget := maxRunPhysicalNameLen - len(suffix) - 1
	if nameBudget < 1 {
		suffix = suffix[:maxRunPhysicalNameLen-2]
		nameBudget = 1
	}
	prefix := strings.Trim(logicalName, "-")
	if len(prefix) > nameBudget {
		prefix = strings.TrimRight(prefix[:nameBudget], "-")
	}
	return prefix + "-" + suffix
}
