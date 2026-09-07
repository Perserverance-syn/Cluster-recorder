package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/Perserverance-syn/Cluster-recorder/internal/diff"
	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
)

// PutSnapshot stores a snapshot and sets its ID.
func (s *DB) PutSnapshot(ctx context.Context, snap *sample.Snapshot) error {
	fields, err := json.Marshal(snap.Fields)
	if err != nil {
		return err
	}
	res, err := s.sql.ExecContext(ctx, `INSERT INTO snapshots (node, source, ts, driver, driver_confidence, driver_reason, fields, baseline)
		VALUES (?,?,?,?,?,?,?,?)`,
		snap.Node, snap.Source, ms(snap.Time), snap.Driver, snap.DriverConfidence, snap.DriverReason, string(fields), snap.Baseline)
	if err != nil {
		return err
	}
	snap.ID, err = res.LastInsertId()
	return err
}

const snapshotCols = `id, node, source, ts, driver, driver_confidence, driver_reason, fields, baseline`

func scanSnapshot(row interface{ Scan(...any) error }) (*sample.Snapshot, error) {
	var snap sample.Snapshot
	var ts int64
	var fields string
	if err := row.Scan(&snap.ID, &snap.Node, &snap.Source, &ts, &snap.Driver, &snap.DriverConfidence, &snap.DriverReason, &fields, &snap.Baseline); err != nil {
		return nil, err
	}
	snap.Time = fromMS(ts)
	if err := json.Unmarshal([]byte(fields), &snap.Fields); err != nil {
		return nil, err
	}
	return &snap, nil
}

// LatestSnapshot returns nil when the node has no snapshot from that source.
func (s *DB) LatestSnapshot(ctx context.Context, node, source string) (*sample.Snapshot, error) {
	snap, err := scanSnapshot(s.sql.QueryRowContext(ctx, `SELECT `+snapshotCols+` FROM snapshots
		WHERE node = ? AND source = ? ORDER BY ts DESC, id DESC LIMIT 1`, node, source))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return snap, err
}

// Baseline returns the most recent healthy baseline snapshot, or nil.
func (s *DB) Baseline(ctx context.Context, node, source string) (*sample.Snapshot, error) {
	snap, err := scanSnapshot(s.sql.QueryRowContext(ctx, `SELECT `+snapshotCols+` FROM snapshots
		WHERE node = ? AND source = ? AND baseline = 1 ORDER BY ts DESC, id DESC LIMIT 1`, node, source))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return snap, err
}

// MarkBaseline flags the latest snapshot for node/source as a healthy
// baseline and keeps only the newest `keep` baselines for it.
func (s *DB) MarkBaseline(ctx context.Context, node, source string, keep int) error {
	if _, err := s.sql.ExecContext(ctx, `UPDATE snapshots SET baseline = 1 WHERE id = (
		SELECT id FROM snapshots WHERE node = ? AND source = ? ORDER BY ts DESC, id DESC LIMIT 1)`, node, source); err != nil {
		return err
	}
	_, err := s.sql.ExecContext(ctx, `UPDATE snapshots SET baseline = 0 WHERE node = ? AND source = ? AND baseline = 1 AND id NOT IN (
		SELECT id FROM snapshots WHERE node = ? AND source = ? AND baseline = 1 ORDER BY ts DESC, id DESC LIMIT ?)`,
		node, source, node, source, keep)
	return err
}

func (s *DB) Snapshots(ctx context.Context, node string, from, to time.Time) ([]sample.Snapshot, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT `+snapshotCols+` FROM snapshots
		WHERE node = ? AND ts >= ? AND ts <= ? ORDER BY ts, id`, node, ms(from), ms(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []sample.Snapshot{}
	for rows.Next() {
		snap, err := scanSnapshot(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *snap)
	}
	return out, rows.Err()
}

// NodeSummary is what /api/v1/nodes lists per node.
type NodeSummary struct {
	Node        string     `json:"node"`
	LastAPI     *time.Time `json:"last_api_snapshot,omitempty"`
	LastAgent   *time.Time `json:"last_agent_snapshot,omitempty"`
	Driver      string     `json:"driver,omitempty"`
	HasBaseline bool       `json:"has_baseline"`
}

func (s *DB) Nodes(ctx context.Context) ([]NodeSummary, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT node,
		MAX(CASE WHEN source = 'api' THEN ts END),
		MAX(CASE WHEN source = 'agent' THEN ts END),
		COALESCE((SELECT driver FROM snapshots a WHERE a.node = snapshots.node AND a.source = 'agent' ORDER BY ts DESC LIMIT 1), ''),
		MAX(baseline)
		FROM snapshots GROUP BY node ORDER BY node`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NodeSummary{}
	for rows.Next() {
		var n NodeSummary
		var api, agent sql.NullInt64
		var baseline int
		if err := rows.Scan(&n.Node, &api, &agent, &n.Driver, &baseline); err != nil {
			return nil, err
		}
		if api.Valid {
			t := fromMS(api.Int64)
			n.LastAPI = &t
		}
		if agent.Valid {
			t := fromMS(agent.Int64)
			n.LastAgent = &t
		}
		n.HasBaseline = baseline == 1
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *DB) PutChanges(ctx context.Context, cs []diff.Change) error {
	for _, c := range cs {
		o, _ := json.Marshal(c.Old)
		n, _ := json.Marshal(c.New)
		if _, err := s.sql.ExecContext(ctx, `INSERT INTO changes (ts, node, source, field, old, new, severity) VALUES (?,?,?,?,?,?,?)`,
			ms(c.Time), c.Node, c.Source, c.Field, string(o), string(n), string(c.Severity)); err != nil {
			return err
		}
	}
	return nil
}

// Changes returns change records for one node, or all nodes when node is "".
func (s *DB) Changes(ctx context.Context, node string, from, to time.Time) ([]diff.Change, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT id, ts, node, source, field, old, new, severity FROM changes
		WHERE ts >= ? AND ts <= ? AND (? = '' OR node = ?) ORDER BY ts, id`, ms(from), ms(to), node, node)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []diff.Change{}
	for rows.Next() {
		var c diff.Change
		var ts int64
		var o, n string
		if err := rows.Scan(&c.ID, &ts, &c.Node, &c.Source, &c.Field, &o, &n, &c.Severity); err != nil {
			return nil, err
		}
		c.Time = fromMS(ts)
		if err := json.Unmarshal([]byte(o), &c.Old); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(n), &c.New); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ChangesOnField counts change records for one field on one node since a time.
func (s *DB) ChangesOnField(ctx context.Context, node, field string, since time.Time) (int, error) {
	var n int
	err := s.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM changes WHERE node = ? AND field = ? AND ts >= ?`, node, field, ms(since)).Scan(&n)
	return n, err
}

// Incident is a detected cluster of failure. Changes are attached at read time.
type Incident struct {
	ID      int64         `json:"id"`
	Opened  time.Time     `json:"opened"`
	Closed  *time.Time    `json:"closed,omitempty"`
	Pods    []string      `json:"pods"`
	Changes []diff.Change `json:"changes,omitempty"`
}

func (s *DB) OpenIncident(ctx context.Context, opened time.Time, pods []string) (int64, error) {
	b, _ := json.Marshal(pods)
	res, err := s.sql.ExecContext(ctx, `INSERT INTO incidents (opened_ts, pods) VALUES (?,?)`, ms(opened), string(b))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *DB) SetIncidentPods(ctx context.Context, id int64, pods []string) error {
	b, _ := json.Marshal(pods)
	_, err := s.sql.ExecContext(ctx, `UPDATE incidents SET pods = ? WHERE id = ?`, string(b), id)
	return err
}

func (s *DB) CloseIncident(ctx context.Context, id int64, at time.Time) error {
	_, err := s.sql.ExecContext(ctx, `UPDATE incidents SET closed_ts = ? WHERE id = ?`, ms(at), id)
	return err
}

// Incidents lists incidents that overlap [from, to]; openOnly restricts to unclosed ones.
func (s *DB) Incidents(ctx context.Context, from, to time.Time, openOnly bool) ([]Incident, error) {
	rows, err := s.sql.QueryContext(ctx, `SELECT id, opened_ts, closed_ts, pods FROM incidents
		WHERE (closed_ts IS NULL OR closed_ts >= ?) AND opened_ts <= ? AND (? = 0 OR closed_ts IS NULL) ORDER BY opened_ts DESC`,
		ms(from), ms(to), openOnly)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Incident{}
	for rows.Next() {
		var inc Incident
		var opened int64
		var closed sql.NullInt64
		var pods string
		if err := rows.Scan(&inc.ID, &opened, &closed, &pods); err != nil {
			return nil, err
		}
		inc.Opened = fromMS(opened)
		if closed.Valid {
			t := fromMS(closed.Int64)
			inc.Closed = &t
		}
		if err := json.Unmarshal([]byte(pods), &inc.Pods); err != nil {
			return nil, err
		}
		out = append(out, inc)
	}
	return out, rows.Err()
}

// AlertsSince counts alert-severity change records cluster-wide since a time.
func (s *DB) AlertsSince(ctx context.Context, since time.Time) (int, error) {
	var n int
	err := s.sql.QueryRowContext(ctx, `SELECT COUNT(*) FROM changes WHERE severity = 'alert' AND ts >= ?`, ms(since)).Scan(&n)
	return n, err
}
