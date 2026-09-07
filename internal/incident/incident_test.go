package incident

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/Perserverance-syn/Cluster-recorder/internal/config"
	"github.com/Perserverance-syn/Cluster-recorder/internal/diff"
	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
	"github.com/Perserverance-syn/Cluster-recorder/internal/store"
)

func setup(t *testing.T) (*Evaluator, *store.DB) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &Evaluator{DB: db, Cfg: config.Defaults(), Log: slog.Default()}, db
}

func crash(t *testing.T, db *store.DB, ns, name string, at time.Time) {
	t.Helper()
	tr := store.PodTransition{Time: at, Namespace: ns, Name: name, Node: "n1", Kind: store.TransitionWaiting, Old: "", New: "CrashLoopBackOff"}
	if err := db.PutPodTransitions(context.Background(), []store.PodTransition{tr}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertPod(context.Background(), store.PodState{Namespace: ns, Name: name, Node: "n1", Phase: "Running", WaitingReason: "CrashLoopBackOff"}, at); err != nil {
		t.Fatal(err)
	}
}

func TestShouldOpen(t *testing.T) {
	now := time.Now()
	three := map[string]time.Time{"a/p1": now, "a/p2": now, "b/p3": now}
	if !ShouldOpen(three, 3, 2) {
		t.Fatal("3 pods / 2 namespaces must open")
	}
	if ShouldOpen(map[string]time.Time{"a/p1": now, "a/p2": now, "a/p3": now}, 3, 2) {
		t.Fatal("one namespace must not open")
	}
	if ShouldOpen(map[string]time.Time{"a/p1": now, "b/p2": now}, 3, 2) {
		t.Fatal("2 pods must not open")
	}
}

func TestIncidentLifecycle(t *testing.T) {
	e, db := setup(t)
	ctx := context.Background()
	t0 := time.Now().UTC().Truncate(time.Millisecond)

	crash(t, db, "a", "p1", t0)
	crash(t, db, "a", "p2", t0.Add(time.Minute))
	if err := e.Tick(ctx, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if open, _ := db.Incidents(ctx, t0.Add(-time.Hour), t0.Add(time.Hour), true); len(open) != 0 {
		t.Fatal("2 pods in 1 namespace must not open an incident")
	}

	crash(t, db, "b", "p3", t0.Add(2*time.Minute))
	if err := e.Tick(ctx, t0.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	open, _ := db.Incidents(ctx, t0.Add(-time.Hour), t0.Add(time.Hour), true)
	if len(open) != 1 || len(open[0].Pods) != 3 || !open[0].Opened.Equal(t0) {
		t.Fatalf("open = %+v", open)
	}

	// A late joiner is folded into the open incident, not a new one.
	crash(t, db, "c", "p4", t0.Add(4*time.Minute))
	_ = e.Tick(ctx, t0.Add(5*time.Minute))
	open, _ = db.Incidents(ctx, t0.Add(-time.Hour), t0.Add(time.Hour), true)
	if len(open) != 1 || len(open[0].Pods) != 4 {
		t.Fatalf("late joiner: %+v", open)
	}

	// Recovery: all Running, but only for 10 minutes -> still open.
	recovered := t0.Add(10 * time.Minute)
	for _, p := range []string{"p1", "p2"} {
		_ = db.UpsertPod(ctx, store.PodState{Namespace: "a", Name: p, Node: "n1", Phase: "Running"}, recovered)
	}
	_ = db.UpsertPod(ctx, store.PodState{Namespace: "b", Name: "p3", Node: "n1", Phase: "Running"}, recovered)
	_ = db.ForgetPod(ctx, "c/p4") // deleted pods count as resolved
	_ = e.Tick(ctx, recovered.Add(10*time.Minute))
	if open, _ = db.Incidents(ctx, t0.Add(-time.Hour), t0.Add(2*time.Hour), true); len(open) != 1 {
		t.Fatal("must stay open until stable for 15m")
	}
	_ = e.Tick(ctx, recovered.Add(16*time.Minute))
	if open, _ = db.Incidents(ctx, t0.Add(-time.Hour), t0.Add(2*time.Hour), true); len(open) != 0 {
		t.Fatal("must close after 15m stable")
	}
}

func TestBaselineOnlyWhenQuiet(t *testing.T) {
	e, db := setup(t)
	ctx := context.Background()
	t0 := time.Now().UTC()
	snap := func(ready string, at time.Time) {
		_ = db.PutSnapshot(ctx, &sample.Snapshot{Node: "n1", Source: "api", Time: at, Fields: map[string]sample.Field{"node.ready": sample.Present(ready)}})
	}
	snap("False", t0)
	_ = e.Tick(ctx, t0.Add(20*time.Minute))
	if b, _ := db.Baseline(ctx, "n1", "api"); b != nil {
		t.Fatal("NotReady node must not be baselined")
	}
	snap("True", t0.Add(time.Minute))
	_ = db.UpsertPod(ctx, store.PodState{Namespace: "a", Name: "p", Node: "n1", Phase: "Running", WaitingReason: "CrashLoopBackOff"}, t0)
	_ = e.Tick(ctx, t0.Add(20*time.Minute))
	if b, _ := db.Baseline(ctx, "n1", "api"); b != nil {
		t.Fatal("crashlooping pod must block baseline")
	}
	_ = db.UpsertPod(ctx, store.PodState{Namespace: "a", Name: "p", Node: "n1", Phase: "Running"}, t0)
	_ = db.PutChanges(ctx, []diff.Change{{Time: t0.Add(10 * time.Minute), Node: "n1", Source: "agent", Field: "pod_bridge", Old: sample.Present("cni0"), New: sample.Absent(), Severity: diff.SeverityAlert}})
	_ = e.Tick(ctx, t0.Add(20*time.Minute))
	if b, _ := db.Baseline(ctx, "n1", "api"); b != nil {
		t.Fatal("an alert in the window must block baseline")
	}
	_ = e.Tick(ctx, t0.Add(40*time.Minute))
	b, _ := db.Baseline(ctx, "n1", "api")
	if b == nil || !b.Fields["node.ready"].Equal(sample.Present("True")) {
		t.Fatalf("quiet cluster must baseline latest snapshot, got %+v", b)
	}
}
