package store

import (
	"context"
	"database/sql"
	"time"
)

// Event is a Kubernetes event, retained past the API server's TTL.
type Event struct {
	UID          string    `json:"uid"`
	Namespace    string    `json:"namespace"`
	Name         string    `json:"name"`
	Type         string    `json:"type"`
	Reason       string    `json:"reason"`
	Message      string    `json:"message"`
	Kind         string    `json:"kind"`
	ObjNamespace string    `json:"obj_namespace"`
	ObjName      string    `json:"obj_name"`
	Node         string    `json:"node"`
	Count        int32     `json:"count"`
	First        time.Time `json:"first"`
	Last         time.Time `json:"last"`
}

func (s *DB) PutEvent(ctx context.Context, e Event) error {
	_, err := s.sql.ExecContext(ctx, `INSERT OR REPLACE INTO events
		(uid, namespace, name, type, reason, message, kind, obj_namespace, obj_name, node, count, first_ts, last_ts)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.UID, e.Namespace, e.Name, e.Type, e.Reason, e.Message, e.Kind, e.ObjNamespace, e.ObjName, e.Node, e.Count, ms(e.First), ms(e.Last))
	return err
}

type EventQuery struct {
	Namespace string
	Node      string
	From, To  time.Time
	Limit     int
}

func (s *DB) Events(ctx context.Context, q EventQuery) ([]Event, error) {
	if q.Limit <= 0 {
		q.Limit = 1000
	}
	rows, err := s.sql.QueryContext(ctx, `SELECT uid, namespace, name, type, reason, message, kind, obj_namespace, obj_name, node, count, first_ts, last_ts
		FROM events WHERE last_ts >= ? AND last_ts <= ? AND (? = '' OR namespace = ?) AND (? = '' OR node = ?)
		ORDER BY last_ts LIMIT ?`,
		ms(q.From), ms(q.To), q.Namespace, q.Namespace, q.Node, q.Node, q.Limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var first, last int64
		if err := rows.Scan(&e.UID, &e.Namespace, &e.Name, &e.Type, &e.Reason, &e.Message, &e.Kind, &e.ObjNamespace, &e.ObjName, &e.Node, &e.Count, &first, &last); err != nil {
			return nil, err
		}
		e.First, e.Last = fromMS(first), fromMS(last)
		out = append(out, e)
	}
	return out, rows.Err()
}

// PodTransition is one observed change in a pod: a phase change, a container
// restart, or a container waiting-reason change (e.g. "" -> CrashLoopBackOff).
type PodTransition struct {
	ID        int64     `json:"id,omitempty"`
	Time      time.Time `json:"time"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Node      string    `json:"node"`
	Kind      string    `json:"kind"`
	Old       string    `json:"old"`
	New       string    `json:"new"`
	Detail    string    `json:"detail,omitempty"`
}

const (
	TransitionPhase   = "phase"
	TransitionRestart = "restart"
	TransitionWaiting = "waiting"
)

func (s *DB) PutPodTransitions(ctx context.Context, ts []PodTransition) error {
	for _, t := range ts {
		if _, err := s.sql.ExecContext(ctx, `INSERT INTO pod_transitions (ts, namespace, name, node, kind, old, new, detail) VALUES (?,?,?,?,?,?,?,?)`,
			ms(t.Time), t.Namespace, t.Name, t.Node, t.Kind, t.Old, t.New, t.Detail); err != nil {
			return err
		}
	}
	return nil
}

// PodTransitions returns transitions for one node, or all nodes when node is "".
func (s *DB) PodTransitions(ctx context.Context, node string, from, to time.Time) ([]PodTransition, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT id, ts, namespace, name, node, kind, old, new, detail FROM pod_transitions
		WHERE ts >= ? AND ts <= ? AND (? = '' OR node = ?) ORDER BY ts, id`, ms(from), ms(to), node, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PodTransition{}
	for rows.Next() {
		var t PodTransition
		var ts int64
		if err := rows.Scan(&t.ID, &ts, &t.Namespace, &t.Name, &t.Node, &t.Kind, &t.Old, &t.New, &t.Detail); err != nil {
			return nil, err
		}
		t.Time = fromMS(ts)
		out = append(out, t)
	}
	return out, rows.Err()
}

// CrashLoopsSince returns the pods that entered CrashLoopBackOff at or after
// since, with the time they entered it.
func (s *DB) CrashLoopsSince(ctx context.Context, since time.Time) (map[string]time.Time, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT namespace || '/' || name, MIN(ts) FROM pod_transitions
		WHERE kind = ? AND new = 'CrashLoopBackOff' AND ts >= ? GROUP BY 1`, TransitionWaiting, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]time.Time{}
	for rows.Next() {
		var key string
		var ts int64
		if err := rows.Scan(&key, &ts); err != nil {
			return nil, err
		}
		out[key] = fromMS(ts)
	}
	return out, rows.Err()
}

// RestartsSince counts container restart transitions cluster-wide since a time.
func (s *DB) RestartsSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM pod_transitions WHERE kind = ? AND ts >= ?`, TransitionRestart, ms(since)).Scan(&n)
	return n, err
}

// PodState is the current state of one pod.
type PodState struct {
	Namespace     string    `json:"namespace"`
	Name          string    `json:"name"`
	Node          string    `json:"node"`
	Phase         string    `json:"phase"`
	WaitingReason string    `json:"waiting_reason"`
	Restarts      int32     `json:"restarts"`
	Since         time.Time `json:"since"` // when the current phase/waiting state began
}

func (p PodState) Key() string { return p.Namespace + "/" + p.Name }

// UpsertPod records the pod's current state. since_ts is preserved when the
// phase and waiting reason are unchanged, so "Running for 15 minutes" is answerable.
func (s *DB) UpsertPod(ctx context.Context, p PodState, now time.Time) error {
	_, err := s.sql.ExecContext(ctx, `INSERT INTO pods (key, namespace, name, node, phase, waiting_reason, restarts, since_ts)
		VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET node = excluded.node, phase = excluded.phase, waiting_reason = excluded.waiting_reason,
		restarts = excluded.restarts,
		since_ts = CASE WHEN pods.phase = excluded.phase AND pods.waiting_reason = excluded.waiting_reason THEN pods.since_ts ELSE excluded.since_ts END`,
		p.Key(), p.Namespace, p.Name, p.Node, p.Phase, p.WaitingReason, p.Restarts, ms(now))
	return err
}

// ForgetPod drops a pod from the current-state table after it leaves the
// cluster. Its transitions and events are kept.
func (s *DB) ForgetPod(ctx context.Context, key string) error {
	_, err := s.sql.ExecContext(ctx, `DELETE FROM pods WHERE key = ?`, key)
	return err
}

// Pod returns nil when the pod is not (or no longer) known.
func (s *DB) Pod(ctx context.Context, key string) (*PodState, error) {
	var p PodState
	var since int64
	err := s.sql.QueryRowContext(ctx, `SELECT namespace, name, node, phase, waiting_reason, restarts, since_ts FROM pods WHERE key = ?`, key).
		Scan(&p.Namespace, &p.Name, &p.Node, &p.Phase, &p.WaitingReason, &p.Restarts, &since)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Since = fromMS(since)
	return &p, nil
}

// CrashLoopingPods lists pods currently in CrashLoopBackOff.
func (s *DB) CrashLoopingPods(ctx context.Context) ([]string, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT key FROM pods WHERE waiting_reason = 'CrashLoopBackOff' ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
