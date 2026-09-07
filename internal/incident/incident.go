// Package incident groups pod failures into incidents and marks healthy
// baselines. It runs periodically over the store; nothing here is event-driven.
package incident

import (
	"context"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/Perserverance-syn/Cluster-recorder/internal/config"
	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
	"github.com/Perserverance-syn/Cluster-recorder/internal/store"
)

const (
	tick          = 30 * time.Second
	baselinesKept = 4
	crashLoop     = "CrashLoopBackOff"
)

type Evaluator struct {
	DB  *store.DB
	Cfg config.Config
	Log *slog.Logger
}

func (e *Evaluator) Run(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if err := e.Tick(ctx, now); err != nil {
				e.Log.Error("incident tick", "err", err)
			}
		}
	}
}

// Tick runs one evaluation at time now.
func (e *Evaluator) Tick(ctx context.Context, now time.Time) error {
	if err := e.incidents(ctx, now); err != nil {
		return err
	}
	return e.baseline(ctx, now)
}

// ShouldOpen is the grouping rule: >= podThreshold pods across >= nsThreshold
// namespaces entered CrashLoopBackOff within the window.
func ShouldOpen(crashing map[string]time.Time, podThreshold, nsThreshold int) bool {
	if len(crashing) < podThreshold {
		return false
	}
	ns := map[string]struct{}{}
	for key := range crashing {
		ns[strings.SplitN(key, "/", 2)[0]] = struct{}{}
	}
	return len(ns) >= nsThreshold
}

func (e *Evaluator) incidents(ctx context.Context, now time.Time) error {
	crashing, err := e.DB.CrashLoopsSince(ctx, now.Add(-e.Cfg.IncidentWindow))
	if err != nil {
		return err
	}
	open, err := e.DB.Incidents(ctx, now.Add(-24*time.Hour), now, true)
	if err != nil {
		return err
	}
	if len(open) == 0 {
		if !ShouldOpen(crashing, e.Cfg.IncidentPodThresh, e.Cfg.IncidentNSThresh) {
			return nil
		}
		opened, pods := now, []string{}
		for key, t := range crashing {
			pods = append(pods, key)
			if t.Before(opened) {
				opened = t
			}
		}
		sort.Strings(pods)
		id, err := e.DB.OpenIncident(ctx, opened, pods)
		if err != nil {
			return err
		}
		e.Log.Warn("incident opened", "id", id, "opened", opened, "pods", pods)
		return nil
	}

	inc := open[0]
	have := map[string]struct{}{}
	for _, p := range inc.Pods {
		have[p] = struct{}{}
	}
	added := false
	for key := range crashing {
		if _, ok := have[key]; !ok {
			inc.Pods = append(inc.Pods, key)
			added = true
		}
	}
	if added {
		sort.Strings(inc.Pods)
		if err := e.DB.SetIncidentPods(ctx, inc.ID, inc.Pods); err != nil {
			return err
		}
	}
	// Close when every affected pod has been Running (or is gone) for the stable window.
	for _, key := range inc.Pods {
		p, err := e.DB.Pod(ctx, key)
		if err != nil {
			return err
		}
		if p == nil {
			continue
		}
		if p.Phase != "Running" || p.WaitingReason != "" || now.Sub(p.Since) < e.Cfg.BaselineStable {
			return nil
		}
	}
	e.Log.Info("incident closed", "id", inc.ID)
	return e.DB.CloseIncident(ctx, inc.ID, now)
}

// baseline marks the latest snapshot per node as a healthy reference when the
// cluster has been quiet for the stable window: every node Ready with no
// readiness flaps, no pod in CrashLoopBackOff, no restarts, no alerts.
func (e *Evaluator) baseline(ctx context.Context, now time.Time) error {
	since := now.Add(-e.Cfg.BaselineStable)
	nodes, err := e.DB.Nodes(ctx)
	if err != nil || len(nodes) == 0 {
		return err
	}
	if cl, err := e.DB.CrashLoopingPods(ctx); err != nil || len(cl) > 0 {
		return err
	}
	if recent, err := e.DB.CrashLoopsSince(ctx, since); err != nil || len(recent) > 0 {
		return err
	}
	if n, err := e.DB.RestartsSince(ctx, since); err != nil || n > 0 {
		return err
	}
	// An alert is a field that was present and vanished. A snapshot taken while
	// something is missing is not a healthy reference.
	if n, err := e.DB.AlertsSince(ctx, since); err != nil || n > 0 {
		return err
	}
	for _, n := range nodes {
		latest, err := e.DB.LatestSnapshot(ctx, n.Node, sample.SourceAPI)
		if err != nil {
			return err
		}
		if latest == nil || !latest.Fields["node.ready"].Equal(sample.Present("True")) {
			return nil
		}
		if flaps, err := e.DB.ChangesOnField(ctx, n.Node, "node.ready", since); err != nil || flaps > 0 {
			return err
		}
	}
	for _, n := range nodes {
		for _, src := range []string{sample.SourceAPI, sample.SourceAgent} {
			latest, err := e.DB.LatestSnapshot(ctx, n.Node, src)
			if err != nil {
				return err
			}
			if latest == nil || latest.Baseline {
				continue
			}
			if err := e.DB.MarkBaseline(ctx, n.Node, src, baselinesKept); err != nil {
				return err
			}
			e.Log.Info("baseline marked", "node", n.Node, "source", src, "snapshot", latest.ID)
		}
	}
	return nil
}
