// Package sample defines the unit of everything the recorder stores, diffs and
// serves: a Field with one of four explicit states, and a Snapshot of Fields
// for one node at one instant.
package sample

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// FieldState is one of exactly four states. Collapsing any two of them produces
// false alarms or missed outages. See docs/ARCHITECTURE.md.
type FieldState string

const (
	StatePresent       FieldState = "present"        // read OK, has a value
	StateAbsent        FieldState = "absent"         // read OK, genuinely does not exist
	StateNotApplicable FieldState = "not_applicable" // this CNI has no such concept
	StateUnknown       FieldState = "unknown"        // sampling failed
)

// Field is the unit of sampling, storage, diffing and display.
// Never marshal a Field as a bare value.
type Field struct {
	State FieldState `json:"state"`
	// Value is set only when State == StatePresent.
	Value any `json:"value,omitempty"`
	// Reason explains StateUnknown (the error) or StateNotApplicable.
	Reason string `json:"reason,omitempty"`
}

func Present(v any) Field               { return Field{State: StatePresent, Value: v} }
func Absent() Field                     { return Field{State: StateAbsent} }
func NotApplicable(reason string) Field { return Field{State: StateNotApplicable, Reason: reason} }
func Unknown(reason string) Field       { return Field{State: StateUnknown, Reason: reason} }

// Validate rejects the two shapes that silently corrupt the model: a present
// field with no value, and a non-present field carrying one.
func (f Field) Validate() error {
	switch f.State {
	case StatePresent:
		if f.Value == nil {
			return errors.New("present field has no value")
		}
	case StateAbsent, StateNotApplicable, StateUnknown:
		if f.Value != nil {
			return fmt.Errorf("%s field carries a value", f.State)
		}
	default:
		return fmt.Errorf("invalid field state %q", f.State)
	}
	return nil
}

// Equal compares state and, for present fields, value. Values are compared by
// their JSON encoding so a snapshot round-tripped through storage (where ints
// come back as float64) still compares equal to a freshly sampled one.
func (f Field) Equal(o Field) bool {
	if f.State != o.State {
		return false
	}
	if f.State != StatePresent {
		return true
	}
	a, _ := json.Marshal(f.Value)
	b, _ := json.Marshal(o.Value)
	return string(a) == string(b)
}

const (
	SourceAPI   = "api"   // collector-side, from Node.status via the API server
	SourceAgent = "agent" // tier 2 node agent, via netlink
)

// Snapshot is one node's complete field set at one instant, from one source.
type Snapshot struct {
	ID     int64     `json:"id,omitempty"`
	Node   string    `json:"node"`
	Source string    `json:"source"`
	Time   time.Time `json:"time"`
	// Driver metadata is recorded with every agent snapshot so "which driver
	// was active?" is answerable after the fact. Empty for api snapshots.
	Driver           string           `json:"driver,omitempty"`
	DriverConfidence int              `json:"driver_confidence,omitempty"`
	DriverReason     string           `json:"driver_reason,omitempty"`
	Fields           map[string]Field `json:"fields"`
	Baseline         bool             `json:"baseline,omitempty"`
}

func (s *Snapshot) Validate() error {
	if s.Node == "" {
		return errors.New("snapshot has no node")
	}
	if s.Source != SourceAPI && s.Source != SourceAgent {
		return fmt.Errorf("snapshot source must be %q or %q, got %q", SourceAPI, SourceAgent, s.Source)
	}
	if s.Time.IsZero() {
		return errors.New("snapshot has no time")
	}
	if len(s.Fields) == 0 {
		return errors.New("snapshot has no fields")
	}
	for k, f := range s.Fields {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("field %q: %w", k, err)
		}
	}
	return nil
}

// FieldsEqual reports whether two field maps are identical key by key.
func FieldsEqual(a, b map[string]Field) bool {
	if len(a) != len(b) {
		return false
	}
	for k, av := range a {
		bv, ok := b[k]
		if !ok || !av.Equal(bv) {
			return false
		}
	}
	return true
}
