// Command agent is the tier-2 node sampler. Runs as a DaemonSet with
// hostNetwork and hostPID, no Kubernetes API access, read-only netlink.
//
//	agent          sample every interval and post to the collector
//	agent --once   print one snapshot as JSON to stdout and exit (verify by hand on a node)
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Perserverance-syn/Cluster-recorder/internal/agent"
	"github.com/Perserverance-syn/Cluster-recorder/internal/netlink"
)

var version = "dev"

func main() {
	cfg, err := agent.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal: configuration invalid:\n"+err.Error())
		os.Exit(1)
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	a := agent.New(cfg, netlink.Host{}, log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if len(os.Args) > 1 && os.Args[1] == "--once" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(a.Snapshot(ctx))
		return
	}
	log.Info("cluster-recorder agent starting", "version", version, "node", cfg.Node, "collector", cfg.CollectorURL, "interval", cfg.Interval)
	a.Run(ctx)
}
