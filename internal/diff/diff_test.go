package diff

import (
	"testing"
	"time"

	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
)

// Every row of the transition table. The unknown->absent row is the one that
// regresses most easily and whose failure mode is a flood of false alarms.
func TestClassify(t *testing.T) {
	p, p2, a, na, u := sample.Present("x"), sample.Present("y"), sample.Absent(), sample.NotApplicable("n"), sample.Unknown("e")
	cases := []struct {
		name     string
		old, new sample.Field
		want     Severity
		emit     bool
	}{
		{"present->absent", p, a, SeverityAlert, true},
		{"present->present changed", p, p2, SeverityInfo, true},
		{"present->present same", p, p, "", false},
		{"absent->present", a, p, SeverityRecovery, true},
		{"absent->absent", a, a, "", false},
		{"present->unknown", p, u, SeverityDegraded, true},
		{"absent->unknown", a, u, SeverityDegraded, true},
		{"unknown->absent", u, a, "", false},
		{"unknown->present", u, p, "", false},
		{"unknown->unknown", u, u, "", false},
		{"present->na", p, na, "", false},
		{"na->present", na, p, "", false},
		{"na->absent", na, a, "", false},
		{"absent->na", a, na, "", false},
		{"na->unknown", na, u, "", false},
	}
	for _, c := range cases {
		sev, emit := Classify(c.old, c.new)
		if emit != c.emit || sev != c.want {
			t.Errorf("%s: got (%q,%v) want (%q,%v)", c.name, sev, emit, c.want, c.emit)
		}
	}
}

func TestSnapshots(t *testing.T) {
	now := time.Now()
	prev := &sample.Snapshot{Node: "n1", Source: "api", Time: now.Add(-time.Minute), Fields: map[string]sample.Field{
		"a": sample.Present(1), "b": sample.Present("old"), "gone": sample.Present(true),
	}}
	cur := &sample.Snapshot{Node: "n1", Source: "api", Time: now, Fields: map[string]sample.Field{
		"a": sample.Absent(), "b": sample.Present("new"), "new": sample.Present(1),
	}}
	got := Snapshots(prev, cur)
	want := map[string]Severity{"a": SeverityAlert, "b": SeverityInfo, "gone": SeverityDegraded}
	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d: %+v", len(got), len(want), got)
	}
	for _, c := range got {
		if want[c.Field] != c.Severity {
			t.Errorf("%s: got %s want %s", c.Field, c.Severity, want[c.Field])
		}
		if c.Node != "n1" || !c.Time.Equal(now) {
			t.Errorf("change %s lost node/time", c.Field)
		}
	}
	if Snapshots(nil, cur) != nil {
		t.Fatal("first snapshot must produce no changes")
	}
}
