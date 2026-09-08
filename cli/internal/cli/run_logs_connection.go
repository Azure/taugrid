// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	logConnectionConfigMap = "tau-log-connection"
	logConnectionSchema    = "tau.logs.connection.v1"
)

type logConnection struct {
	Schema   string `json:"schema"`
	Endpoint string `json:"endpoint"`
	Database string `json:"database"`
	Cluster  string `json:"cluster"`
}

func fetchLogConnection(ctx context.Context, runner kubeRawRunner, namespace string) (logConnection, error) {
	if runner == nil {
		return logConnection{}, fmt.Errorf("no Kubernetes connection available for log discovery")
	}
	if strings.TrimSpace(namespace) == "" {
		return logConnection{}, fmt.Errorf("no system namespace available for log discovery")
	}
	raw, err := runner.Raw(ctx, []string{"-n", namespace, "get", "configmap", logConnectionConfigMap, "-o", "json"}, nil)
	if err != nil {
		return logConnection{}, fmt.Errorf("read logging ConfigMap %s/%s: %w", namespace, logConnectionConfigMap, err)
	}
	var configMap corev1.ConfigMap
	if err := json.Unmarshal([]byte(raw), &configMap); err != nil {
		return logConnection{}, fmt.Errorf("decode logging ConfigMap %s/%s: %w", namespace, logConnectionConfigMap, err)
	}
	if configMap.Name != logConnectionConfigMap || configMap.Namespace != namespace {
		return logConnection{}, fmt.Errorf("logging ConfigMap identity mismatch: expected %s/%s, got %s/%s", namespace, logConnectionConfigMap, configMap.Namespace, configMap.Name)
	}
	payload := configMap.Data["connection.json"]
	if strings.TrimSpace(payload) == "" {
		return logConnection{}, fmt.Errorf("logging ConfigMap %s/%s is missing data[connection.json]", namespace, logConnectionConfigMap)
	}
	var connection logConnection
	decoder := json.NewDecoder(strings.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&connection); err != nil {
		return logConnection{}, fmt.Errorf("decode logging connection in %s/%s: %w", namespace, logConnectionConfigMap, err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return logConnection{}, fmt.Errorf("logging connection in %s/%s must contain exactly one JSON object", namespace, logConnectionConfigMap)
	}
	if connection.Schema != logConnectionSchema {
		return logConnection{}, fmt.Errorf("unsupported logging connection schema %q in %s/%s; expected %q", connection.Schema, namespace, logConnectionConfigMap, logConnectionSchema)
	}
	return connection, nil
}

func resolveTerminalLogConnection(ctx context.Context, opts runLogsOptions, hooks runLogsHooks) (runLogsOptions, error) {
	missing := missingLogConnectionFlags(opts)
	if len(missing) > 0 {
		connection, err := hooks.resolveLogConnection(ctx)
		if err != nil {
			return opts, fmt.Errorf("discover historical log connection: %w; supply %s or ask the platform administrator to configure taugrid-core logging and grant get access to ConfigMap %s in the selected cluster's system namespace", err, strings.Join(missing, ", "), logConnectionConfigMap)
		}
		if strings.TrimSpace(opts.KustoEndpoint) == "" {
			opts.KustoEndpoint = connection.Endpoint
		}
		if strings.TrimSpace(opts.KustoDatabase) == "" {
			opts.KustoDatabase = connection.Database
		}
		if strings.TrimSpace(opts.KustoCluster) == "" {
			opts.KustoCluster = connection.Cluster
		}
	}
	if missing := missingLogConnectionFlags(opts); len(missing) > 0 {
		return opts, fmt.Errorf("historical log connection is incomplete; configure taugrid-core logging in the selected cluster's system namespace or supply %s", strings.Join(missing, ", "))
	}
	endpoint, err := url.Parse(strings.TrimSpace(opts.KustoEndpoint))
	if err != nil || endpoint.Scheme != "https" || endpoint.Hostname() == "" || endpoint.User != nil ||
		(endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" {
		return opts, fmt.Errorf("historical log connection endpoint must be an absolute HTTPS ADX query URL without credentials, query, fragment, or path; correct logging.endpoint or --kusto-endpoint")
	}
	return opts, nil
}

func missingLogConnectionFlags(opts runLogsOptions) []string {
	var missing []string
	if strings.TrimSpace(opts.KustoCluster) == "" {
		missing = append(missing, "--kusto-cluster")
	}
	if strings.TrimSpace(opts.KustoEndpoint) == "" {
		missing = append(missing, "--kusto-endpoint")
	}
	if strings.TrimSpace(opts.KustoDatabase) == "" {
		missing = append(missing, "--kusto-database")
	}
	return missing
}
