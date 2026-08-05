// Command gpu-health-checker provides DCGM-based GPU health monitoring for NPD.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/collector"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/config"
	dcgmclient "github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/dcgm"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/fieldnames"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/reader/checks"
	"github.com/Azure/taugrid/monitoring/gpu-health-checker/internal/store"
)

const (
	defaultConfig    = "/etc/gpu-health-checker/config.yaml"
	defaultDB        = "/var/run/gpu-health/health.db"
	defaultInterval  = 10 * time.Second
	defaultRetention = 30 * time.Minute

	dcgmMaxRetries   = 10
	dcgmInitialDelay = 2 * time.Second
	dcgmMaxDelay     = 60 * time.Second
)

func main() {
	err := newRootCmd().Execute()
	var ec *exitCodeError
	if errors.As(err, &ec) {
		os.Exit(ec.code)
	}
	if err != nil {
		os.Exit(1)
	}
}

// exitCodeError carries an NPD-compatible exit code (0/1/2) out of a cobra
// RunE so that deferred cleanup (DB close, cancel funcs) runs before the
// process exits.
type exitCodeError struct {
	code int
}

func (e *exitCodeError) Error() string { return fmt.Sprintf("exit code %d", e.code) }

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "gpu-health-checker",
		Short: "DCGM-based GPU health monitoring for NPD",
	}
	root.AddCommand(newCollectCmd(), newReadCmd(), newStatusCmd(), newListCmd())
	return root
}

// --- collect ---

func newCollectCmd() *cobra.Command {
	var (
		cfgPath   string
		dbPath    string
		interval  time.Duration
		retention time.Duration
		verbose   bool
	)

	cmd := &cobra.Command{
		Use:   "collect",
		Short: "Start the collector daemon",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				slog.Error("load config", "error", err)
				return err
			}

			ctx, cancel := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer cancel()

			client, err := dcgmclient.NewClient()
			if err != nil {
				slog.Warn("initial DCGM init failed, retrying with backoff", "error", err)
				client, err = retryDCGMInit(ctx)
				if err != nil {
					slog.Error("DCGM init failed after retries", "error", err)
					return err
				}
			}
			defer func() { _ = client.Close() }()

			db, err := store.Open(dbPath)
			if err != nil {
				slog.Error("open database", "error", err)
				return err
			}
			defer func() { _ = db.Close() }()

			c := collector.New(client, db, cfg, interval, retention, verbose)
			if err := c.Run(ctx); err != nil && ctx.Err() == nil {
				slog.Error("collector failed", "error", err)
				return err
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&cfgPath, "config", defaultConfig, "config file path")
	cmd.Flags().StringVar(&dbPath, "db", defaultDB, "SQLite database path")
	cmd.Flags().DurationVar(&interval, "collect-interval", defaultInterval, "collection interval")
	cmd.Flags().DurationVar(&retention, "retention", defaultRetention, "data retention duration")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "verbose logging")

	return cmd
}

// retryDCGMInit attempts to initialize DCGM with exponential backoff.
// This handles transient DCGM unavailability during driver reloads.
func retryDCGMInit(ctx context.Context) (*dcgmclient.Client, error) {
	delay := dcgmInitialDelay
	for attempt := 1; attempt <= dcgmMaxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		slog.Info("retrying DCGM init", "attempt", attempt, "maxRetries", dcgmMaxRetries)
		client, err := dcgmclient.NewClient()
		if err == nil {
			slog.Info("DCGM init succeeded", "attempt", attempt)
			return client, nil
		}
		slog.Warn("DCGM init retry failed", "attempt", attempt, "error", err)
		delay *= 2
		if delay > dcgmMaxDelay {
			delay = dcgmMaxDelay
		}
	}
	return nil, fmt.Errorf("DCGM init failed after %d retries", dcgmMaxRetries)
}

// --- read ---

func newReadCmd() *cobra.Command {
	var (
		cfgPath string
		dbPath  string
	)

	cmd := &cobra.Command{
		Use:           "read <check|all>",
		Short:         "Run a reader health check (invoked by NPD)",
		Args:          cobra.ExactArgs(1),
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			checkName := args[0]

			cfg, err := config.Load(cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			db, err := store.OpenReadOnly(dbPath)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer func() { _ = db.Close() }()

			ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
			defer cancel()

			if checkName == "all" {
				return &exitCodeError{code: runAllReaders(ctx, db, cfg)}
			}

			r := checks.ByName(checkName)
			if r == nil {
				fmt.Fprintf(os.Stderr, "unknown check: %s\nAvailable: %s\n", checkName, availableChecks())
				// Exit 2: treat unknown check as a hard failure so operators
				// notice misconfiguration rather than seeing it flap as a
				// transient NPD warning.
				return &exitCodeError{code: 2}
			}

			result, err := r.Read(ctx, db, cfg)
			if err != nil {
				return fmt.Errorf("%s: %w", r.Name(), err)
			}

			fmt.Println(result.Message)
			return &exitCodeError{code: result.ExitCode}
		},
	}

	cmd.Flags().StringVar(&cfgPath, "config", defaultConfig, "config file path")
	cmd.Flags().StringVar(&dbPath, "db", defaultDB, "SQLite database path")

	return cmd
}

func runAllReaders(ctx context.Context, db *store.DB, cfg *config.Config) int {
	worstCode := 0
	for _, r := range checks.All() {
		result, err := r.Read(ctx, db, cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", r.Name(), err)
			if worstCode < 1 {
				worstCode = 1
			}
			continue
		}
		if result.ExitCode > 0 {
			fmt.Printf("[%s] %s\n", r.Name(), result.Message)
		}
		if result.ExitCode > worstCode {
			worstCode = result.ExitCode
		}
	}
	if worstCode == 0 {
		fmt.Println("all checks passed")
	}
	return worstCode
}

func availableChecks() string {
	var names []string
	for _, r := range checks.All() {
		names = append(names, r.Name())
	}
	return strings.Join(names, ", ")
}

// --- status ---

func newStatusCmd() *cobra.Command {
	var dbPath string

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show collected GPU data (info, samples, health checks)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			db, err := store.OpenReadOnly(dbPath)
			if err != nil {
				return fmt.Errorf("open database: %w", err)
			}
			defer func() { _ = db.Close() }()

			ctx := cmd.Context()

			// GPU info
			gpuInfo, err := db.QueryAllGPUInfo(ctx)
			if err != nil {
				return fmt.Errorf("query gpu_info: %w", err)
			}
			fmt.Println("=== GPU Info ===")
			if len(gpuInfo) == 0 {
				fmt.Println("  (none)")
			}
			for _, g := range gpuInfo {
				fmt.Printf("  GPU %d: %s  UUID=%s  PCI=%s  VBIOS=%s  Driver=%s\n",
					g.GPU, g.Name, g.UUID, g.PCIBus, g.VBIOS, g.Driver)
			}

			// Latest samples per GPU per field
			fmt.Println("\n=== Latest Samples ===")
			var unsupportedFields []string
			for _, field := range fieldnames.AllFields() {
				samples, err := db.QueryLatestSamples(ctx, field)
				if err != nil {
					fmt.Printf("  %-25s   error: %v\n", field, err)
					continue
				}
				if len(samples) == 0 {
					unsupportedFields = append(unsupportedFields, field)
					continue
				}
				for _, s := range samples {
					ts := time.Unix(s.Timestamp, 0).Format("15:04:05")
					fmt.Printf("  GPU %d  %-25s = %-12.0f  (%s)\n", s.GPU, s.Field, s.Value, ts)
				}
			}
			if len(unsupportedFields) > 0 {
				fmt.Println("\n=== Unsupported / No Data ===")
				for _, field := range unsupportedFields {
					fmt.Printf("  %-25s   NO_DATA\n", field)
				}
			}

			// Latest health checks
			fmt.Println("\n=== Health Checks ===")
			checks, err := db.QueryLatestHealthChecks(ctx)
			if err != nil {
				return fmt.Errorf("query health_checks: %w", err)
			}
			if len(checks) == 0 {
				fmt.Println("  (none)")
			}
			for _, c := range checks {
				ts := time.Unix(c.Timestamp, 0).Format("15:04:05")
				fmt.Printf("  GPU %d  %-10s  %-8s  %s  (%s)\n", c.GPU, c.System, c.Status, c.Message, ts)
			}

			// DB stats
			latest, _ := db.LatestTimestamp()
			if latest > 0 {
				age := time.Since(time.Unix(latest, 0)).Round(time.Second)
				fmt.Printf("\nNewest sample: %s (%s ago)\n",
					time.Unix(latest, 0).Format("2006-01-02 15:04:05"), age)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&dbPath, "db", defaultDB, "SQLite database path")

	return cmd
}

// --- list ---

func newListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List available reader checks",
		Run: func(_ *cobra.Command, _ []string) {
			fmt.Println("Available health checks:")
			for _, r := range checks.All() {
				fmt.Printf("  %s\n", r.Name())
			}
		},
	}
}
