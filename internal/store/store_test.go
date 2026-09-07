package store

import (
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Perserverance-syn/Cluster-recorder/internal/diff"
	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
)

func open(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "t.db"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSnapshotsChangesAndTimeline(t *testing.T) {
	db, ctx := open(t), context.Background()
	now := time.Now().UTC().Truncate(time.Millisecond)

	if got, _ := db.LatestSnapshot(ctx, "n1", "api"); got != nil {
		t.Fatal("expected no snapshot")
	}
	s1 := &sample.Snapshot{Node: "n1", Source: "api", Time: now.Add(-2 * time.Minute), Fields: map[string]sample.Field{"node.ready": sample.Present("True")}}
	s2 := &sample.Snapshot{Node: "n1", Source: "api", Time: now, Fields: map[string]sample.Field{"node.ready": sample.Present("False")}}
	for _, s := range []*sample.Snapshot{s1, s2} {
		if err := db.PutSnapshot(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	latest, _ := db.LatestSnapshot(ctx, "n1", "api")
	if latest.ID != s2.ID || !latest.Fields["node.ready"].Equal(sample.Present("False")) {
		t.Fatalf("latest = %+v", latest)
	}

	changes := diff.Snapshots(s1, s2)
	if err := db.PutChanges(ctx, changes); err != nil {
		t.Fatal(err)
	}
	if err := db.PutEvent(ctx, Event{UID: "e1", Namespace: "kube-system", Name: "x", Type: "Warning", Reason: "NodeNotReady", Kind: "Node", ObjName: "n1", Node: "n1", Count: 1, First: now, Last: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := db.PutPodTransitions(ctx, []PodTransition{{Time: now.Add(-time.Minute), Namespace: "a", Name: "p", Node: "n1", Kind: TransitionPhase, Old: "Pending", New: "Running"}}); err != nil {
		t.Fatal(err)
	}

	tl, err := db.Timeline(ctx, "n1", now.Add(-time.Hour), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, e := range tl {
		kinds = append(kinds, e.Kind)
	}
	if strings.Join(kinds, ",") != "pod_transition,change,event" {
		t.Fatalf("timeline order wrong: %v", kinds)
	}
	if other, _ := db.Timeline(ctx, "n2", now.Add(-time.Hour), now.Add(time.Hour)); len(other) != 0 {
		t.Fatal("timeline leaked across nodes")
	}
}

func TestBaselineKeepsNewestFour(t *testing.T) {
	db, ctx := open(t), context.Background()
	now := time.Now()
	for i := 0; i < 6; i++ {
		s := &sample.Snapshot{Node: "n1", Source: "api", Time: now.Add(time.Duration(i) * time.Minute), Fields: map[string]sample.Field{"k": sample.Present(i)}}
		if err := db.PutSnapshot(ctx, s); err != nil {
			t.Fatal(err)
		}
		if err := db.MarkBaseline(ctx, "n1", "api", 4); err != nil {
			t.Fatal(err)
		}
	}
	b, _ := db.Baseline(ctx, "n1", "api")
	if !b.Fields["k"].Equal(sample.Present(5)) {
		t.Fatalf("baseline should be newest, got %+v", b.Fields["k"])
	}
	all, _ := db.Snapshots(ctx, "n1", now.Add(-time.Hour), now.Add(time.Hour))
	n := 0
	for _, s := range all {
		if s.Baseline {
			n++
		}
	}
	if n != 4 {
		t.Fatalf("want 4 baselines, got %d", n)
	}
	// Prune must not touch baselines even when they are older than retention.
	if err := db.Prune(ctx, now.Add(time.Hour), Retention{Events: time.Second, Changes: time.Second, Snapshots: time.Second}); err != nil {
		t.Fatal(err)
	}
	all, _ = db.Snapshots(ctx, "n1", now.Add(-time.Hour), now.Add(time.Hour))
	if len(all) != 4 {
		t.Fatalf("prune should leave the 4 baselines, left %d", len(all))
	}
}

func TestPodStateSincePreserved(t *testing.T) {
	db, ctx := open(t), context.Background()
	t0 := time.Now().UTC().Truncate(time.Millisecond)
	p := PodState{Namespace: "a", Name: "p", Node: "n1", Phase: "Running"}
	if err := db.UpsertPod(ctx, p, t0); err != nil {
		t.Fatal(err)
	}
	p.Restarts = 3 // restarts alone do not reset the clock
	if err := db.UpsertPod(ctx, p, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ := db.Pod(ctx, "a/p")
	if !got.Since.Equal(t0) || got.Restarts != 3 {
		t.Fatalf("since should be preserved: %+v", got)
	}
	p.WaitingReason = "CrashLoopBackOff"
	_ = db.UpsertPod(ctx, p, t0.Add(2*time.Minute))
	got, _ = db.Pod(ctx, "a/p")
	if !got.Since.Equal(t0.Add(2 * time.Minute)) {
		t.Fatalf("since should reset on state change: %+v", got)
	}
	cl, _ := db.CrashLoopingPods(ctx)
	if len(cl) != 1 || cl[0] != "a/p" {
		t.Fatalf("crashlooping = %v", cl)
	}
	_ = db.ForgetPod(ctx, "a/p")
	if got, _ := db.Pod(ctx, "a/p"); got != nil {
		t.Fatal("pod should be forgotten")
	}
}

func TestIncidents(t *testing.T) {
	db, ctx := open(t), context.Background()
	now := time.Now()
	id, err := db.OpenIncident(ctx, now, []string{"a/p1", "b/p2"})
	if err != nil {
		t.Fatal(err)
	}
	open, _ := db.Incidents(ctx, now.Add(-time.Hour), now.Add(time.Hour), true)
	if len(open) != 1 || open[0].ID != id || len(open[0].Pods) != 2 {
		t.Fatalf("open = %+v", open)
	}
	_ = db.CloseIncident(ctx, id, now.Add(time.Minute))
	if open, _ = db.Incidents(ctx, now.Add(-time.Hour), now.Add(time.Hour), true); len(open) != 0 {
		t.Fatal("incident should be closed")
	}
	all, _ := db.Incidents(ctx, now.Add(-time.Hour), now.Add(time.Hour), false)
	if len(all) != 1 || all[0].Closed == nil {
		t.Fatalf("all = %+v", all)
	}
}

func TestSizeCapDropsSnapshotsFirst(t *testing.T) {
	db, ctx := open(t), context.Background()
	now := time.Now()
	big := strings.Repeat("x", 4096)
	for i := 0; i < 200; i++ {
		_ = db.PutSnapshot(ctx, &sample.Snapshot{Node: "n1", Source: "api", Time: now.Add(time.Duration(i) * time.Second), Fields: map[string]sample.Field{"pad": sample.Present(big)}})
		_ = db.PutEvent(ctx, Event{UID: "e" + string(rune('A'+i%26)) + string(rune(i)), Message: big, First: now, Last: now})
	}
	before, _ := db.SizeBytes(ctx)
	limit := before * 6 / 10 // snapshots are ~half the data, so dropping them alone satisfies this
	if err := db.EnforceSizeCap(ctx, limit); err != nil {
		t.Fatal(err)
	}
	after, _ := db.SizeBytes(ctx)
	if after > limit {
		t.Fatalf("still over cap: %d > %d", after, limit)
	}
	snaps, _ := db.Snapshots(ctx, "n1", now.Add(-time.Hour), now.Add(time.Hour))
	events, _ := db.Events(ctx, EventQuery{From: now.Add(-time.Hour), To: now.Add(time.Hour)})
	if len(snaps) != 0 || len(events) == 0 {
		t.Fatalf("snapshots must go before events: %d snapshots, %d events left", len(snaps), len(events))
	}
}
