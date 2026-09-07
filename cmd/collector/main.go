// Command collector is the tier-1 recorder: informers over Events, Pods and
// Nodes, a diff engine, SQLite storage, and the read-only HTTP API.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Perserverance-syn/Cluster-recorder/internal/api"
	"github.com/Perserverance-syn/Cluster-recorder/internal/config"
	"github.com/Perserverance-syn/Cluster-recorder/internal/incident"
	"github.com/Perserverance-syn/Cluster-recorder/internal/informers"
	"github.com/Perserverance-syn/Cluster-recorder/internal/store"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return fmt.Errorf("configuration invalid:\n%w", err)
	}
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	log.Info("cluster-recorder collector starting", "version", version)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(cfg.StorageDSN, log)
	if err != nil {
		return fmt.Errorf("open storage %s: %w", cfg.StorageDSN, err)
	}
	defer db.Close()

	client, err := kubeClient()
	if err != nil {
		return err
	}
	collector, err := informers.New(client, db, log)
	if err != nil {
		return err
	}
	go func() {
		if err := collector.Run(ctx); err != nil {
			log.Error("informers stopped", "err", err)
			stop()
		}
	}()
	go (&incident.Evaluator{DB: db, Cfg: cfg, Log: log}).Run(ctx)
	go maintenance(ctx, db, cfg, log)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           (&api.Server{DB: db, Ready: collector.Ready, Hints: collector.Hints, Log: log}).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Info("api listening", "addr", cfg.ListenAddr)
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// kubeClient uses the in-cluster service account, else KUBECONFIG / ~/.kube/config
// so the collector runs unchanged from a laptop against any cluster.
func kubeClient() (kubernetes.Interface, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		path := os.Getenv("KUBECONFIG")
		if path == "" {
			home, _ := os.UserHomeDir()
			path = filepath.Join(home, ".kube", "config")
		}
		cfg, err = clientcmd.BuildConfigFromFlags("", path)
		if err != nil {
			return nil, fmt.Errorf("no in-cluster config and no kubeconfig at %s: %w", path, err)
		}
	}
	cfg.UserAgent = "cluster-recorder/" + version
	return kubernetes.NewForConfig(cfg)
}

// maintenance enforces the size cap every 5 minutes and retention hourly.
func maintenance(ctx context.Context, db *store.DB, cfg config.Config, log *slog.Logger) {
	capBytes := int64(cfg.StorageMaxMB) * 1024 * 1024
	retention := store.Retention{Events: cfg.RetentionEvents, Changes: cfg.RetentionChanges, Snapshots: cfg.RetentionSnapshots}
	capTick := time.NewTicker(5 * time.Minute)
	pruneTick := time.NewTicker(time.Hour)
	defer capTick.Stop()
	defer pruneTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-capTick.C:
			if err := db.EnforceSizeCap(ctx, capBytes); err != nil {
				log.Error("size cap", "err", err)
			}
		case now := <-pruneTick.C:
			if err := db.Prune(ctx, now, retention); err != nil {
				log.Error("prune", "err", err)
			}
		}
	}
}
