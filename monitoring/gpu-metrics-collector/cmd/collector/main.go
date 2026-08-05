package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/conditions"
	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/config"
	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/rules"
	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/scraper"
	"github.com/Azure/taugrid/monitoring/gpu-metrics-collector/internal/state"
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "error", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "/etc/gpu-metrics-collector/rules.yaml", "Path to rules config")
	nodeName := flag.String("node-name", "", "Node name (or set NODE_NAME env var)")
	scrapeInterval := flag.Duration("scrape-interval", 15*time.Second, "Metrics scrape interval")
	stateDir := flag.String("state-dir", "/var/lib/gpu-metrics-collector", "Directory for persisting state across restarts")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	if *nodeName == "" {
		*nodeName = os.Getenv("NODE_NAME")
	}
	if *nodeName == "" {
		return fmt.Errorf("node name required: use --node-name or NODE_NAME env var")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}
	slog.Info("loaded config", "rules", len(cfg.Rules), "targets", len(cfg.ScrapeTargets))

	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("building in-cluster config: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("creating kubernetes client: %w", err)
	}

	sc := scraper.New(cfg.ScrapeTargets)
	engine := rules.NewEngine(cfg.Rules)
	writer := conditions.NewWriter(clientset, *nodeName)

	// Restore state from previous run if available.
	if snap, err := state.Load(*stateDir, 30*time.Minute); err != nil {
		slog.Warn("failed to load state", "error", err)
	} else if snap != nil {
		engine.RestoreState(snap.History, snap.Pending)
		writer.RestoreLastStatus(snap.LastStatus)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	// Jitter startup so collectors across the fleet don't all start in lockstep.
	jitter := time.Duration(rand.Int64N(int64(*scrapeInterval)))
	slog.Info("starting gpu-metrics-collector",
		"node", *nodeName,
		"scrapeInterval", scrapeInterval.String(),
		"rules", len(cfg.Rules),
		"startupJitter", jitter.String())

	select {
	case <-time.After(jitter):
	case <-ctx.Done():
		return nil
	}

	ticker := time.NewTicker(*scrapeInterval)
	defer ticker.Stop()

	// Run immediately on start, then on interval.
	if err := collect(ctx, sc, engine, writer); err != nil {
		slog.Error("collection failed", "error", err)
	}
	saveState(engine, writer, *stateDir)

	for {
		select {
		case <-ctx.Done():
			slog.Info("shutting down")
			saveState(engine, writer, *stateDir)
			return nil
		case <-ticker.C:
			if err := collect(ctx, sc, engine, writer); err != nil {
				slog.Error("collection failed", "error", err)
			}
			saveState(engine, writer, *stateDir)
		}
	}
}

func saveState(engine *rules.Engine, writer *conditions.Writer, dir string) {
	history, pending := engine.ExportState()
	snap := &state.Snapshot{
		History:    history,
		Pending:    pending,
		LastStatus: writer.ExportLastStatus(),
	}
	if err := state.Save(dir, snap); err != nil {
		slog.Warn("failed to save state", "error", err)
	}
}

func collect(ctx context.Context, sc *scraper.Scraper, engine *rules.Engine, writer *conditions.Writer) error {
	metrics, err := sc.Scrape(ctx)
	if err != nil {
		return fmt.Errorf("scraping metrics: %w", err)
	}

	results := engine.Evaluate(metrics)

	if err := writer.WriteConditions(ctx, results); err != nil {
		return fmt.Errorf("writing conditions: %w", err)
	}

	return nil
}
