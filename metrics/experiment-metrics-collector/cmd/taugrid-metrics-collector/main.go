// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Azure/taugrid/core/version"
	collector "github.com/Azure/taugrid/metrics/experiment-metrics-collector"
)

const binaryName = "taugrid-metrics-collector"

const (
	deliveryModeADXRequired = "adx-required"
)

type repeated []string

func (v *repeated) String() string { return strings.Join(*v, ",") }
func (v *repeated) Set(value string) error {
	*v = append(*v, value)
	return nil
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) > 0 {
		switch args[0] {
		case "-h", "--help", "help":
			printUsage(stdout)
			return nil
		case "-v", "--version", "version":
			fmt.Fprintln(stdout, version.Info(binaryName))
			return nil
		}
	}
	if len(args) == 0 || args[0] != "collect" {
		return fmt.Errorf("usage: %s collect [flags]", binaryName)
	}
	flags := flag.NewFlagSet("collect", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var histories, rawTags repeated
	var options collector.Options
	var deliveryMode, adxClusterURI, adxDatabase, adxTable, adxMapping, adxClientID string
	var adxMaxAttempts int
	var adxBackoff, adxFinalTimeout time.Duration
	flags.StringVar(&options.Run, "run", "", "run id")
	flags.StringVar(&options.Project, "project", "", "project id")
	flags.StringVar(&options.Experiment, "experiment", "", "experiment id")
	flags.StringVar(&options.Group, "group", "", "run group id")
	flags.StringVar(&options.Source, "source", "stellar-online", "metric source")
	flags.StringVar(&options.Out, "out", "", "spool and checkpoint directory")
	flags.Var(&histories, "history", "append-only JSONL history path or glob (repeatable)")
	flags.Var(&rawTags, "tag", "tag key=value (repeatable)")
	flags.StringVar(&options.CompletionFile, "completion-file", "", "workload completion sentinel")
	flags.DurationVar(&options.Interval, "interval", time.Minute, "watch polling interval")
	flags.StringVar(&deliveryMode, "delivery-mode", deliveryModeADXRequired, "delivery mode: adx-required")
	flags.StringVar(&adxClusterURI, "adx-cluster-uri", "", "Azure Data Explorer cluster URI")
	flags.StringVar(&adxDatabase, "adx-database", "", "Azure Data Explorer database")
	flags.StringVar(&adxTable, "adx-table", "TauExpMetricEventsV1", "Azure Data Explorer metric event table")
	flags.StringVar(&adxMapping, "adx-mapping", "TauExpMetricEventsV1Json", "ADX JSON ingestion mapping name")
	flags.StringVar(&adxClientID, "adx-client-id", "", "Azure managed or workload identity client ID")
	flags.BoolVar(&options.BaselineExistingHistory, "baseline-existing-history", false, "checkpoint complete existing history before collecting")
	flags.StringVar(&options.ReadyFile, "ready-file", "", "readiness sentinel")
	flags.StringVar(&options.DoneFile, "done-file", "", "terminal delivery sentinel")
	flags.StringVar(&options.StatusArtifactURI, "status-artifact-uri", "", "terminal artifact URI tag")
	flags.StringVar(&options.StatusCheckpointURI, "status-checkpoint-uri", "", "terminal checkpoint URI tag")
	flags.IntVar(&adxMaxAttempts, "adx-max-attempts", 3, "maximum ADX queued ingestion attempts")
	flags.DurationVar(&adxBackoff, "adx-retry-backoff", time.Second, "initial ADX retry backoff")
	flags.DurationVar(&adxFinalTimeout, "adx-final-status-timeout", 10*time.Minute, "maximum wait for terminal ADX ingestion status")
	flags.BoolVar(&options.Watch, "watch", false, "watch until completion")
	flags.IntVar(&options.MaxIterations, "max-iterations", 0, "maximum watch iterations (tests)")
	if err := flags.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	changed := map[string]bool{}
	flags.Visit(func(value *flag.Flag) {
		changed[value.Name] = true
	})
	if len(histories) == 0 && !changed["history"] {
		if value := strings.TrimSpace(os.Getenv("TAU_METRICS_HISTORY")); value != "" {
			histories = append(histories, value)
		}
	}
	applyStringEnvDefault(changed, "run", &options.Run, "TAU_METRICS_OFFLOAD_RUN")
	applyStringEnvDefault(changed, "project", &options.Project, "TAU_METRICS_OFFLOAD_PROJECT")
	applyStringEnvDefault(changed, "experiment", &options.Experiment, "TAU_METRICS_OFFLOAD_EXPERIMENT")
	applyStringEnvDefault(changed, "group", &options.Group, "TAU_METRICS_OFFLOAD_GROUP")
	applyStringEnvDefault(changed, "source", &options.Source, "TAU_METRICS_OFFLOAD_SOURCE")
	applyStringEnvDefault(changed, "out", &options.Out, "TAU_METRICS_OFFLOAD_OUT")
	applyStringEnvDefault(changed, "completion-file", &options.CompletionFile, "TAU_METRICS_OFFLOAD_COMPLETION_FILE")
	applyStringEnvDefault(changed, "status-artifact-uri", &options.StatusArtifactURI, "TAU_METRICS_OFFLOAD_ARTIFACT_URI")
	applyStringEnvDefault(changed, "status-checkpoint-uri", &options.StatusCheckpointURI, "TAU_METRICS_OFFLOAD_CHECKPOINT_URI")
	applyStringEnvDefault(changed, "delivery-mode", &deliveryMode, "TAU_METRICS_OFFLOAD_DELIVERY_MODE")
	applyStringEnvDefault(changed, "adx-cluster-uri", &adxClusterURI, "TAU_METRICS_OFFLOAD_ADX_CLUSTER_URI")
	applyStringEnvDefault(changed, "adx-database", &adxDatabase, "TAU_METRICS_OFFLOAD_ADX_DATABASE")
	applyStringEnvDefault(changed, "adx-table", &adxTable, "TAU_METRICS_OFFLOAD_ADX_TABLE")
	applyStringEnvDefault(changed, "adx-mapping", &adxMapping, "TAU_METRICS_OFFLOAD_ADX_MAPPING")
	applyStringEnvDefault(changed, "adx-client-id", &adxClientID, "TAU_METRICS_OFFLOAD_ADX_CLIENT_ID")
	for _, durationDefault := range []struct {
		flagName string
		target   *time.Duration
		envName  string
	}{
		{"adx-retry-backoff", &adxBackoff, "TAU_METRICS_OFFLOAD_ADX_RETRY_BACKOFF"},
		{"adx-final-status-timeout", &adxFinalTimeout, "TAU_METRICS_OFFLOAD_ADX_FINAL_STATUS_TIMEOUT"},
	} {
		if err := applyDurationEnvDefault(changed, durationDefault.flagName, durationDefault.target, durationDefault.envName); err != nil {
			return err
		}
	}
	for _, intDefault := range []struct {
		flagName string
		target   *int
		envName  string
	}{
		{"adx-max-attempts", &adxMaxAttempts, "TAU_METRICS_OFFLOAD_ADX_MAX_ATTEMPTS"},
	} {
		if err := applyIntEnvDefault(changed, intDefault.flagName, intDefault.target, intDefault.envName); err != nil {
			return err
		}
	}
	if !changed["interval"] {
		if value := strings.TrimSpace(os.Getenv("TAU_METRICS_OFFLOAD_INTERVAL")); value != "" {
			parsed, err := time.ParseDuration(value)
			if err != nil {
				return fmt.Errorf("TAU_METRICS_OFFLOAD_INTERVAL: %w", err)
			}
			options.Interval = parsed
		}
	}
	if len(rawTags) == 0 && !changed["tag"] {
		for _, value := range strings.Split(os.Getenv("TAU_METRICS_OFFLOAD_TAGS"), ",") {
			if value = strings.TrimSpace(value); value != "" {
				rawTags = append(rawTags, value)
			}
		}
	}
	if adxMaxAttempts < 0 || adxMaxAttempts > collector.MaxADXQueuedAttempts {
		return fmt.Errorf("--adx-max-attempts must be between 0 and %d", collector.MaxADXQueuedAttempts)
	}
	if adxBackoff < 0 || adxFinalTimeout <= 0 {
		return fmt.Errorf("ADX backoff must be nonnegative and final status timeout must be positive")
	}
	options.History = histories
	options.Tags = map[string]string{}
	for _, raw := range rawTags {
		key, value, ok := strings.Cut(raw, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			return fmt.Errorf("--tag must be key=value")
		}
		options.Tags[key] = strings.TrimSpace(value)
	}
	deliveryMode = strings.ToLower(strings.TrimSpace(deliveryMode))
	switch deliveryMode {
	case deliveryModeADXRequired:
	default:
		return fmt.Errorf("--delivery-mode must be adx-required")
	}
	adxValues := []string{adxClusterURI, adxDatabase, adxTable, adxMapping, adxClientID}
	for index, name := range []string{"--adx-cluster-uri", "--adx-database", "--adx-table", "--adx-mapping", "--adx-client-id"} {
		if strings.TrimSpace(adxValues[index]) == "" {
			return fmt.Errorf("%s is required with --delivery-mode=%s", name, deliveryMode)
		}
	}
	sink, err := collector.NewADXQueuedSink(collector.ADXQueuedConfig{
		ClusterURI: adxClusterURI, Database: adxDatabase, Table: adxTable, IngestionMapping: adxMapping,
		ClientID: adxClientID, MaxAttempts: adxMaxAttempts, RetryBackoff: adxBackoff,
		FinalStatusTimeout: adxFinalTimeout,
	}, nil)
	if err != nil {
		return err
	}
	options.Sinks = append(options.Sinks, sink)
	runner, err := collector.New(options)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, err := runner.Run(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(result)
}

func applyStringEnvDefault(changed map[string]bool, flagName string, target *string, envName string) {
	if changed[flagName] {
		return
	}
	if value := strings.TrimSpace(os.Getenv(envName)); value != "" {
		*target = value
	}
}

func applyDurationEnvDefault(changed map[string]bool, flagName string, target *time.Duration, envName string) error {
	if changed[flagName] {
		return nil
	}
	value := strings.TrimSpace(os.Getenv(envName))
	if value == "" {
		return nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return fmt.Errorf("%s: %w", envName, err)
	}
	*target = parsed
	return nil
}

func applyIntEnvDefault(changed map[string]bool, flagName string, target *int, envName string) error {
	if changed[flagName] {
		return nil
	}
	value := strings.TrimSpace(os.Getenv(envName))
	if value == "" {
		return nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fmt.Errorf("%s: %w", envName, err)
	}
	*target = parsed
	return nil
}

func printUsage(w io.Writer) {
	fmt.Fprintf(w, `Usage:
  %s collect [flags]
  %s --help
  %s --version

Collect immutable experiment metric history into a typed, restart-safe spool
and optionally deliver it through Prometheus remote-write and ADX queued ingestion.
`, binaryName, binaryName, binaryName)
}
