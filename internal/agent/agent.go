// Package agent is the tier-2 node sampler: read the node every interval,
// post to the collector, buffer when the collector is away. Stateless apart
// from the buffer and the pinned CNI driver. It must survive the outage it
// is recording: never block, never crash on collector downtime.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/Perserverance-syn/Cluster-recorder/internal/cni"
	"github.com/Perserverance-syn/Cluster-recorder/internal/host"
	"github.com/Perserverance-syn/Cluster-recorder/internal/netlink"
	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
)

type Config struct {
	CollectorURL  string
	Node          string
	Driver        string // "auto" or a driver name
	CNIDir        string
	ProcRoot      string
	HostRoot      string
	PodCIDR       string // override; discovery via collector hints is preferred
	Interval      time.Duration
	BufferSize    int
	RedetectEvery time.Duration
}

func FromEnv() (Config, error) {
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	c := Config{
		CollectorURL: get("COLLECTOR_URL", "http://recorder.recorder.svc:8080"),
		Node:         os.Getenv("NODE_NAME"),
		Driver:       get("CNI_DRIVER", "auto"),
		CNIDir:       get("CNI_CONF_DIR", "/etc/cni/net.d"),
		ProcRoot:     get("PROC_ROOT", "/proc"),
		HostRoot:     get("HOST_ROOT", "/host"),
		PodCIDR:      os.Getenv("POD_CIDR"),
		Interval:     30 * time.Second, BufferSize: 100, RedetectEvery: 10 * time.Minute,
	}
	var errs []error
	if c.Node == "" {
		errs = append(errs, errors.New("NODE_NAME is required (use the downward API: spec.nodeName)"))
	}
	if _, err := url.Parse(c.CollectorURL); err != nil {
		errs = append(errs, fmt.Errorf("COLLECTOR_URL: %w", err))
	}
	if v := os.Getenv("SAMPLE_INTERVAL"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 5 {
			errs = append(errs, fmt.Errorf("SAMPLE_INTERVAL: %q must be an integer number of seconds >= 5", v))
		} else {
			c.Interval = time.Duration(n) * time.Second
		}
	}
	if v := os.Getenv("AGENT_BUFFER"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("AGENT_BUFFER: %q must be a positive integer", v))
		} else {
			c.BufferSize = n
		}
	}
	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return c, nil
}

// Hints is what the collector knows that the agent cannot read without API
// access: DaemonSets for CNI detection and this node's pod CIDRs.
type Hints struct {
	DaemonSets []string `json:"daemonsets"`
	PodCIDRs   []string `json:"pod_cidrs"`
}

type Agent struct {
	Cfg    Config
	NL     netlink.Netlink
	Log    *slog.Logger
	Client *http.Client
	Now    func() time.Time

	choice     cni.Choice
	detectedAt time.Time
	hints      Hints
	buffer     []sample.Snapshot
}

func New(cfg Config, nl netlink.Netlink, log *slog.Logger) *Agent {
	return &Agent{Cfg: cfg, NL: nl, Log: log, Client: &http.Client{Timeout: 10 * time.Second}, Now: time.Now}
}

// Run samples every interval until ctx ends. The first sample happens immediately.
func (a *Agent) Run(ctx context.Context) {
	t := time.NewTicker(a.Cfg.Interval)
	defer t.Stop()
	for {
		a.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (a *Agent) tick(ctx context.Context) {
	snap := a.Snapshot(ctx)
	a.buffer = append(a.buffer, *snap)
	if len(a.buffer) > a.Cfg.BufferSize {
		a.Log.Warn("buffer full, dropping oldest sample", "size", a.Cfg.BufferSize)
		a.buffer = a.buffer[len(a.buffer)-a.Cfg.BufferSize:]
	}
	// Flush oldest first so the collector sees samples in order.
	for len(a.buffer) > 0 {
		if err := a.post(ctx, &a.buffer[0]); err != nil {
			a.Log.Warn("collector unreachable, buffering", "buffered", len(a.buffer), "err", err)
			return
		}
		a.buffer = a.buffer[1:]
	}
}

// Buffered is the number of samples waiting for the collector.
func (a *Agent) Buffered() int { return len(a.buffer) }

// Snapshot reads the node once. Detection is refreshed on the first call and
// every RedetectEvery; a changed answer is recorded by the collector as a
// change on the "driver" field.
func (a *Agent) Snapshot(ctx context.Context) *sample.Snapshot {
	now := a.Now()
	a.refreshHints(ctx)
	view, viewErr := a.NL.View(ctx)
	if a.choice.Driver == nil || now.Sub(a.detectedAt) >= a.Cfg.RedetectEvery {
		dctx := cni.DetectContext{CNIConfigs: cni.ParseCNIDir(a.Cfg.CNIDir), DaemonSets: a.hints.DaemonSets}
		if view != nil {
			dctx.Interfaces = view.LinkNames()
		}
		choice, err := cni.Choose(a.Cfg.Driver, dctx)
		if err != nil {
			// A bad forced name is a config error; fall back loudly rather than stop sampling.
			a.Log.Error("driver selection", "err", err)
			choice, _ = cni.Choose("auto", dctx)
		}
		if a.choice.Driver == nil || a.choice.Driver.Name() != choice.Driver.Name() {
			a.Log.Info("cni driver selected", "driver", choice.Driver.Name(), "confidence", choice.Confidence, "reason", choice.Reason)
			a.choice = choice // keeps the old driver instance (and its pinned mode) when the name is unchanged
		}
		a.detectedAt = now
	}
	var nl netlink.Netlink = netlink.Fixture{V: view}
	if viewErr != nil {
		nl = netlink.Failing{Err: viewErr}
	}
	res := cni.Sample(ctx, a.choice.Driver, a.choice.Confidence, a.choice.Reason, nl, a.podCIDRs())
	fields := res.Fields
	for k, f := range host.Fields(a.Cfg.ProcRoot, a.Cfg.HostRoot) {
		fields[k] = f
	}
	reason := res.Reason
	if res.Mode != "" {
		reason += " mode=" + res.Mode
	}
	return &sample.Snapshot{Node: a.Cfg.Node, Source: sample.SourceAgent, Time: now.UTC(),
		Driver: res.Driver, DriverConfidence: res.Confidence, DriverReason: reason, Fields: fields}
}

// podCIDRs follows the discovery order: node podCIDRs from the collector
// (which reads Node.spec.podCIDRs, then the controller-manager cluster CIDR),
// then the explicit override. Nothing is ever guessed.
func (a *Agent) podCIDRs() []string {
	if len(a.hints.PodCIDRs) > 0 {
		return a.hints.PodCIDRs
	}
	if a.Cfg.PodCIDR != "" {
		return []string{a.Cfg.PodCIDR}
	}
	return nil
}

func (a *Agent) refreshHints(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.Cfg.CollectorURL+"/api/v1/agent/hints?node="+url.QueryEscape(a.Cfg.Node), nil)
	if err != nil {
		return
	}
	resp, err := a.Client.Do(req)
	if err != nil {
		return // keep the last hints; the buffer handles the rest
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return
	}
	var h Hints
	if json.NewDecoder(resp.Body).Decode(&h) == nil {
		a.hints = h
	}
}

func (a *Agent) post(ctx context.Context, snap *sample.Snapshot) error {
	body, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.Cfg.CollectorURL+"/api/v1/agent/snapshots", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("collector returned %d: %s", resp.StatusCode, msg)
	}
	return nil
}
