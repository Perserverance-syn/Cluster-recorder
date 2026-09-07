// Package diff turns two snapshots of the same node into change records.
// The transition table here is the engine's entire logic.
package diff

import (
	"sort"
	"time"

	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
)

type Severity string

const (
	SeverityAlert    Severity = "alert"    // present -> absent
	SeverityInfo     Severity = "info"     // present -> present, value changed
	SeverityRecovery Severity = "recovery" // absent -> present
	SeverityDegraded Severity = "degraded" // anything -> unknown: sampling problem, not cluster problem
)

type Change struct {
	ID       int64        `json:"id,omitempty"`
	Time     time.Time    `json:"time"`
	Node     string       `json:"node"`
	Source   string       `json:"source"`
	Field    string       `json:"field"`
	Old      sample.Field `json:"old"`
	New      sample.Field `json:"new"`
	Severity Severity     `json:"severity"`
}

// Classify applies the transition table. ok=false means "emit nothing".
//
//	present -> absent                  alert
//	present -> present (value differs) info
//	absent  -> present                 recovery
//	anything -> unknown                degraded
//	unknown -> anything                nothing  (cannot claim something vanished that was never seen)
//	any <-> not_applicable             nothing
func Classify(old, new sample.Field) (Severity, bool) {
	if old.State == sample.StateNotApplicable || new.State == sample.StateNotApplicable {
		return "", false
	}
	if old.State == sample.StateUnknown {
		return "", false
	}
	if new.State == sample.StateUnknown {
		return SeverityDegraded, true
	}
	switch {
	case old.State == sample.StatePresent && new.State == sample.StateAbsent:
		return SeverityAlert, true
	case old.State == sample.StateAbsent && new.State == sample.StatePresent:
		return SeverityRecovery, true
	case old.State == sample.StatePresent && new.State == sample.StatePresent && !old.Equal(new):
		return SeverityInfo, true
	}
	return "", false
}

// Snapshots diffs cur against prev field by field. A key missing from one side
// is treated as unknown ("not sampled") on that side, so a field that stops
// being reported shows up as degraded and one that starts being reported
// emits nothing until it has a clean previous read.
func Snapshots(prev, cur *sample.Snapshot) []Change {
	if prev == nil || cur == nil {
		return nil
	}
	keys := map[string]struct{}{}
	for k := range prev.Fields {
		keys[k] = struct{}{}
	}
	for k := range cur.Fields {
		keys[k] = struct{}{}
	}
	var out []Change
	for k := range keys {
		o, ok := prev.Fields[k]
		if !ok {
			o = sample.Unknown("not sampled")
		}
		n, ok := cur.Fields[k]
		if !ok {
			n = sample.Unknown("not sampled")
		}
		sev, emit := Classify(o, n)
		if !emit {
			continue
		}
		out = append(out, Change{Time: cur.Time, Node: cur.Node, Source: cur.Source, Field: k, Old: o, New: n, Severity: sev})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}
