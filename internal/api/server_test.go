package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
	"github.com/Perserverance-syn/Cluster-recorder/internal/store"
)

func server(t *testing.T) (*httptest.Server, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer((&Server{DB: db, Ready: func() bool { return true }, Log: slog.Default()}).Handler())
	t.Cleanup(func() { ts.Close(); db.Close() })
	return ts, db
}

func get(t *testing.T, url string, into any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if into != nil {
		if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

func TestParseTime(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if got, _ := parseTime("-10m", now); !got.Equal(now.Add(-10 * time.Minute)) {
		t.Errorf("relative: %v", got)
	}
	if got, _ := parseTime("2026-01-01T11:00:00Z", now); !got.Equal(now.Add(-time.Hour)) {
		t.Errorf("rfc3339: %v", got)
	}
	if got, _ := parseTime("1767268800", now); !got.Equal(now) {
		t.Errorf("unix: %v", got)
	}
	if _, err := parseTime("yesterday", now); err == nil {
		t.Error("garbage must fail")
	}
}

func TestDegradesWithoutAgentData(t *testing.T) {
	ts, db := server(t)
	ctx := context.Background()
	now := time.Now().UTC()
	_ = db.PutSnapshot(ctx, &sample.Snapshot{Node: "n1", Source: "api", Time: now, Fields: map[string]sample.Field{"node.ready": sample.Present("True")}})

	var tl []store.TimelineEntry
	if code := get(t, ts.URL+"/api/v1/nodes/n1/timeline?from=-1h", &tl); code != 200 {
		t.Fatalf("timeline code %d", code)
	}
	var d struct {
		Sources map[string]sourceDiff `json:"sources"`
	}
	if code := get(t, ts.URL+"/api/v1/nodes/n1/diff?against=baseline", &d); code != 200 {
		t.Fatalf("diff code %d", code)
	}
	if _, ok := d.Sources["agent"]; ok {
		t.Fatal("agent source must be omitted when no agent data")
	}
	if d.Sources["api"].Note == "" {
		t.Fatal("missing baseline must be noted, not errored")
	}
	if code := get(t, ts.URL+"/api/v1/nodes/nope/diff", nil); code != 404 {
		t.Fatalf("unknown node code %d", code)
	}
	if code := get(t, ts.URL+"/api/v1/events?from=tomorrow", nil); code != 400 {
		t.Fatalf("bad time code %d", code)
	}
}

func TestAgentIngestDiffsAndRecordsDriverChange(t *testing.T) {
	ts, db := server(t)
	post := func(body string) (int, map[string]any) {
		resp, err := http.Post(ts.URL+"/api/v1/agent/snapshots", "application/json", bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}
	if code, _ := post(`{"node":"n1","fields":{"pod_bridge":{"state":"present"}}}`); code != 400 {
		t.Fatalf("present without value must be rejected, got %d", code)
	}
	if code, _ := post(`{"node":"n1","driver":"flannel","fields":{"pod_bridge.ipv4":{"state":"present","value":"10.244.2.1"},"overlay_tunnel":{"state":"unknown","reason":"netlink: permission denied"}}}`); code != 202 {
		t.Fatalf("first snapshot code %d", code)
	}
	code, out := post(`{"node":"n1","driver":"generic","fields":{"pod_bridge.ipv4":{"state":"absent"},"overlay_tunnel":{"state":"absent"}}}`)
	if code != 202 || out["changes"].(float64) != 2 {
		t.Fatalf("second snapshot: code %d out %v", code, out)
	}
	changes, _ := db.Changes(context.Background(), "n1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	got := map[string]string{}
	for _, c := range changes {
		got[c.Field] = string(c.Severity)
	}
	// pod_bridge.ipv4 present->absent = alert; driver flannel->generic = info;
	// overlay_tunnel unknown->absent = nothing.
	if got["pod_bridge.ipv4"] != "alert" || got["driver"] != "info" || len(got) != 2 {
		t.Fatalf("changes = %v", got)
	}
	var nodes []store.NodeSummary
	get(t, ts.URL+"/api/v1/nodes", &nodes)
	if len(nodes) != 1 || nodes[0].Driver != "generic" || nodes[0].LastAgent == nil {
		t.Fatalf("nodes = %+v", nodes)
	}
}
