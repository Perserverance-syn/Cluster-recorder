// Package store is the only package that talks to the database. SQLite via
// modernc.org/sqlite (pure Go, no cgo) so the binary stays static and tests
// run anywhere.
package store

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

type DB struct {
	sql *sql.DB
	log *slog.Logger
}

// Open opens (or creates) the SQLite database at path and applies migrations.
func Open(path string, log *slog.Logger) (*DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// ponytail: one connection serialises everything; per-connection WAL
	// readers if read latency under write load ever matters.
	db.SetMaxOpenConns(1)
	for _, p := range []string{
		"PRAGMA auto_vacuum=INCREMENTAL", // must precede table creation to take effect
		"PRAGMA journal_mode=WAL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA synchronous=NORMAL",
	} {
		if _, err := db.Exec(p); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	}
	s := &DB{sql: db, log: log}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *DB) Close() error { return s.sql.Close() }

func (s *DB) migrate() error {
	var version int
	if err := s.sql.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for i, name := range names {
		if i < version {
			continue
		}
		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		if _, err := s.sql.Exec(string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := s.sql.Exec(fmt.Sprintf("PRAGMA user_version=%d", i+1)); err != nil {
			return err
		}
		s.log.Info("applied migration", "name", name)
	}
	return nil
}

func ms(t time.Time) int64     { return t.UnixMilli() }
func fromMS(v int64) time.Time { return time.UnixMilli(v).UTC() }

// Retention is how long each record class is kept.
type Retention struct{ Events, Changes, Snapshots time.Duration }

// Prune deletes records older than their retention window and reclaims the
// freed pages. Baseline snapshots are exempt so the healthy reference survives.
func (s *DB) Prune(ctx context.Context, now time.Time, r Retention) error {
	steps := []struct {
		q   string
		cut time.Time
	}{
		{"DELETE FROM events WHERE last_ts < ?", now.Add(-r.Events)},
		{"DELETE FROM pod_transitions WHERE ts < ?", now.Add(-r.Events)},
		{"DELETE FROM changes WHERE ts < ?", now.Add(-r.Changes)},
		{"DELETE FROM snapshots WHERE ts < ? AND baseline = 0", now.Add(-r.Snapshots)},
		{"DELETE FROM incidents WHERE closed_ts IS NOT NULL AND closed_ts < ?", now.Add(-r.Changes)},
	}
	for _, st := range steps {
		res, err := s.sql.ExecContext(ctx, st.q, ms(st.cut))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			s.log.Info("pruned", "query", st.q, "rows", n)
		}
	}
	if _, err := s.sql.ExecContext(ctx, "PRAGMA incremental_vacuum"); err != nil {
		return err
	}
	_, err := s.sql.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// SizeBytes is the number of bytes in use (pages minus freelist).
func (s *DB) SizeBytes(ctx context.Context) (int64, error) {
	var pages, free, size int64
	for k, dst := range map[string]*int64{"page_count": &pages, "freelist_count": &free, "page_size": &size} {
		if err := s.sql.QueryRowContext(ctx, "PRAGMA "+k).Scan(dst); err != nil {
			return 0, err
		}
	}
	return (pages - free) * size, nil
}

// EnforceSizeCap drops the oldest raw snapshots first, then the oldest
// events, until the database is under maxBytes. A recorder that fills the
// disk and takes down the node it was watching is the worst failure mode.
func (s *DB) EnforceSizeCap(ctx context.Context, maxBytes int64) error {
	dropOrder := []string{
		"DELETE FROM snapshots WHERE baseline = 0 AND id IN (SELECT id FROM snapshots WHERE baseline = 0 ORDER BY ts LIMIT 100)",
		"DELETE FROM events WHERE uid IN (SELECT uid FROM events ORDER BY last_ts LIMIT 100)",
	}
	for {
		used, err := s.SizeBytes(ctx)
		if err != nil || used <= maxBytes {
			return err
		}
		s.log.Warn("database over size cap, dropping oldest records", "used_bytes", used, "cap_bytes", maxBytes)
		dropped := false
		for _, q := range dropOrder {
			res, err := s.sql.ExecContext(ctx, q)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n > 0 {
				dropped = true
				break
			}
		}
		if !dropped {
			return fmt.Errorf("database over cap (%d > %d bytes) with nothing left to drop", used, maxBytes)
		}
		if _, err := s.sql.ExecContext(ctx, "PRAGMA incremental_vacuum"); err != nil {
			return err
		}
	}
}

// TimelineEntry is one row of the merged per-node timeline.
type TimelineEntry struct {
	Time time.Time `json:"time"`
	Kind string    `json:"kind"` // change | event | pod_transition
	Node string    `json:"node"`
	Data any       `json:"data"`
}

// Timeline merges change records, events and pod transitions for one node,
// oldest first. It is the primary read path.
func (s *DB) Timeline(ctx context.Context, node string, from, to time.Time) ([]TimelineEntry, error) {
	out := []TimelineEntry{}
	changes, err := s.Changes(ctx, node, from, to)
	if err != nil {
		return nil, err
	}
	for _, c := range changes {
		out = append(out, TimelineEntry{c.Time, "change", c.Node, c})
	}
	events, err := s.Events(ctx, EventQuery{Node: node, From: from, To: to})
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		out = append(out, TimelineEntry{e.Last, "event", e.Node, e})
	}
	trans, err := s.PodTransitions(ctx, node, from, to)
	if err != nil {
		return nil, err
	}
	for _, t := range trans {
		out = append(out, TimelineEntry{t.Time, "pod_transition", t.Node, t})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}
