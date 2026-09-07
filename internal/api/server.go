// Package api serves the read-only HTTP API plus the one write endpoint the
// node agent posts to. Unauthenticated, ClusterIP-only in v1. See docs/LIMITS.md.
package api

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Perserverance-syn/Cluster-recorder/internal/diff"
	"github.com/Perserverance-syn/Cluster-recorder/internal/informers"
	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
	"github.com/Perserverance-syn/Cluster-recorder/internal/store"
)

//go:embed openapi.yaml
var openAPI []byte

const incidentLookback = 6 * time.Hour

type Server struct {
	DB    *store.DB
	Ready func() bool
	Hints func(node string) (informers.Hints, error) // nil when running without a cluster (tests)
	Log   *slog.Logger
	Now   func() time.Time
}

func (s *Server) Handler() http.Handler {
	if s.Now == nil {
		s.Now = time.Now
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /api/v1/readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.Ready != nil && !s.Ready() {
			writeJSON(w, 503, map[string]string{"status": "syncing"})
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("GET /api/v1/openapi.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/yaml")
		w.Write(openAPI)
	})
	mux.HandleFunc("GET /api/v1/events", s.events)
	mux.HandleFunc("GET /api/v1/nodes", s.nodes)
	mux.HandleFunc("GET /api/v1/nodes/{node}/timeline", s.timeline)
	mux.HandleFunc("GET /api/v1/nodes/{node}/snapshots", s.snapshots)
	mux.HandleFunc("GET /api/v1/nodes/{node}/diff", s.diff)
	mux.HandleFunc("GET /api/v1/incidents", s.incidents)
	mux.HandleFunc("POST /api/v1/agent/snapshots", s.agentSnapshot)
	mux.HandleFunc("GET /api/v1/agent/hints", s.agentHints)
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) fail(w http.ResponseWriter, code int, err error) {
	if code >= 500 {
		s.Log.Error("api", "err", err)
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

// parseTime accepts RFC3339, Unix seconds, or a negative duration relative
// to now ("-10m", "-6h").
func parseTime(v string, now time.Time) (time.Time, error) {
	if strings.HasPrefix(v, "-") {
		d, err := time.ParseDuration(v)
		if err != nil {
			return time.Time{}, fmt.Errorf("%q: not a duration", v)
		}
		return now.Add(d), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return time.Unix(n, 0), nil
	}
	return time.Time{}, fmt.Errorf("%q: use RFC3339, unix seconds, or a relative duration like -10m", v)
}

func (s *Server) timeRange(r *http.Request) (from, to time.Time, err error) {
	now := s.Now()
	from, to = now.Add(-time.Hour), now
	if v := r.URL.Query().Get("from"); v != "" {
		if from, err = parseTime(v, now); err != nil {
			return
		}
	}
	if v := r.URL.Query().Get("to"); v != "" {
		if to, err = parseTime(v, now); err != nil {
			return
		}
	}
	if to.Before(from) {
		err = errors.New("to is before from")
	}
	return
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.timeRange(r)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	out, err := s.DB.Events(r.Context(), store.EventQuery{Namespace: r.URL.Query().Get("namespace"), Node: r.URL.Query().Get("node"), From: from, To: to, Limit: limit})
	if err != nil {
		s.fail(w, 500, err)
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) nodes(w http.ResponseWriter, r *http.Request) {
	out, err := s.DB.Nodes(r.Context())
	if err != nil {
		s.fail(w, 500, err)
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) timeline(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.timeRange(r)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	out, err := s.DB.Timeline(r.Context(), r.PathValue("node"), from, to)
	if err != nil {
		s.fail(w, 500, err)
		return
	}
	writeJSON(w, 200, out)
}

func (s *Server) snapshots(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.timeRange(r)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	out, err := s.DB.Snapshots(r.Context(), r.PathValue("node"), from, to)
	if err != nil {
		s.fail(w, 500, err)
		return
	}
	writeJSON(w, 200, out)
}

type fieldDiff struct {
	Field    string        `json:"field"`
	Baseline *sample.Field `json:"baseline"`
	Current  *sample.Field `json:"current"`
	Changed  bool          `json:"changed"`
}

type sourceDiff struct {
	BaselineTime *time.Time  `json:"baseline_time"`
	CurrentTime  time.Time   `json:"current_time"`
	Driver       string      `json:"driver,omitempty"`
	Fields       []fieldDiff `json:"fields"`
	Note         string      `json:"note,omitempty"`
}

// diff compares the latest snapshot against the last healthy baseline, per
// source. Missing agent data is reported, never an error.
func (s *Server) diff(w http.ResponseWriter, r *http.Request) {
	if a := r.URL.Query().Get("against"); a != "" && a != "baseline" {
		s.fail(w, 400, fmt.Errorf("against=%q unsupported; only baseline", a))
		return
	}
	node := r.PathValue("node")
	out := map[string]sourceDiff{}
	for _, src := range []string{sample.SourceAPI, sample.SourceAgent} {
		cur, err := s.DB.LatestSnapshot(r.Context(), node, src)
		if err != nil {
			s.fail(w, 500, err)
			return
		}
		if cur == nil {
			continue
		}
		base, err := s.DB.Baseline(r.Context(), node, src)
		if err != nil {
			s.fail(w, 500, err)
			return
		}
		sd := sourceDiff{CurrentTime: cur.Time, Driver: cur.Driver, Fields: []fieldDiff{}}
		if base == nil {
			sd.Note = "no healthy baseline recorded yet for this node/source"
		} else {
			sd.BaselineTime = &base.Time
		}
		keys := map[string]struct{}{}
		for k := range cur.Fields {
			keys[k] = struct{}{}
		}
		if base != nil {
			for k := range base.Fields {
				keys[k] = struct{}{}
			}
		}
		for k := range keys {
			fd := fieldDiff{Field: k}
			if base != nil {
				if f, ok := base.Fields[k]; ok {
					fd.Baseline = &f
				}
			}
			if f, ok := cur.Fields[k]; ok {
				fd.Current = &f
			}
			fd.Changed = base != nil && (fd.Baseline == nil || fd.Current == nil || !fd.Baseline.Equal(*fd.Current))
			sd.Fields = append(sd.Fields, fd)
		}
		sortFields(sd.Fields)
		out[src] = sd
	}
	if len(out) == 0 {
		s.fail(w, 404, fmt.Errorf("no snapshots recorded for node %q", node))
		return
	}
	writeJSON(w, 200, map[string]any{"node": node, "sources": out})
}

func sortFields(f []fieldDiff) {
	for i := 1; i < len(f); i++ {
		for j := i; j > 0 && f[j].Field < f[j-1].Field; j-- {
			f[j], f[j-1] = f[j-1], f[j]
		}
	}
}

func (s *Server) incidents(w http.ResponseWriter, r *http.Request) {
	from, to, err := s.timeRange(r)
	if err != nil {
		s.fail(w, 400, err)
		return
	}
	if r.URL.Query().Get("from") == "" {
		from = to.Add(-30 * 24 * time.Hour)
	}
	incs, err := s.DB.Incidents(r.Context(), from, to, r.URL.Query().Get("open") == "true")
	if err != nil {
		s.fail(w, 500, err)
		return
	}
	for i := range incs {
		end := incs[i].Opened
		if incs[i].Closed != nil {
			end = *incs[i].Closed
		}
		incs[i].Changes, err = s.DB.Changes(r.Context(), "", incs[i].Opened.Add(-incidentLookback), end)
		if err != nil {
			s.fail(w, 500, err)
			return
		}
	}
	writeJSON(w, 200, incs)
}

// agentSnapshot accepts a tier-2 node sample, diffs it against the previous
// agent snapshot for that node, and stores both. A driver change between
// snapshots is itself recorded as a change.
func (s *Server) agentSnapshot(w http.ResponseWriter, r *http.Request) {
	var snap sample.Snapshot
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&snap); err != nil {
		s.fail(w, 400, fmt.Errorf("decode: %w", err))
		return
	}
	snap.ID, snap.Source, snap.Baseline = 0, sample.SourceAgent, false
	if snap.Time.IsZero() {
		snap.Time = s.Now()
	}
	if err := snap.Validate(); err != nil {
		s.fail(w, 400, err)
		return
	}
	ctx := r.Context()
	prev, err := s.DB.LatestSnapshot(ctx, snap.Node, sample.SourceAgent)
	if err != nil {
		s.fail(w, 500, err)
		return
	}
	if err := s.DB.PutSnapshot(ctx, &snap); err != nil {
		s.fail(w, 500, err)
		return
	}
	changes := diff.Snapshots(prev, &snap)
	if prev != nil && prev.Driver != snap.Driver {
		changes = append(changes, diff.Change{Time: snap.Time, Node: snap.Node, Source: snap.Source, Field: "driver",
			Old: sample.Present(prev.Driver), New: sample.Present(snap.Driver), Severity: diff.SeverityInfo})
	}
	if err := s.DB.PutChanges(ctx, changes); err != nil {
		s.fail(w, 500, err)
		return
	}
	for _, ch := range changes {
		s.Log.Info("agent change", "node", ch.Node, "field", ch.Field, "severity", ch.Severity, "old", ch.Old, "new", ch.New)
	}
	writeJSON(w, 202, map[string]any{"id": snap.ID, "changes": len(changes)})
}

// agentHints serves CNI detection hints and pod CIDRs to an agent.
func (s *Server) agentHints(w http.ResponseWriter, r *http.Request) {
	node := r.URL.Query().Get("node")
	if node == "" {
		s.fail(w, 400, errors.New("node query parameter is required"))
		return
	}
	if s.Hints == nil {
		writeJSON(w, 200, informers.Hints{DaemonSets: []string{}, PodCIDRs: []string{}})
		return
	}
	h, err := s.Hints(node)
	if err != nil {
		s.fail(w, 500, err)
		return
	}
	writeJSON(w, 200, h)
}
