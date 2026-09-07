-- Timestamps are integer Unix milliseconds, UTC. Field values are JSON
-- objects of the form {"state":"present","value":...}; never bare values.

CREATE TABLE IF NOT EXISTS events (
  uid           TEXT PRIMARY KEY,
  namespace     TEXT NOT NULL,
  name          TEXT NOT NULL,
  type          TEXT NOT NULL,
  reason        TEXT NOT NULL,
  message       TEXT NOT NULL,
  kind          TEXT NOT NULL,
  obj_namespace TEXT NOT NULL,
  obj_name      TEXT NOT NULL,
  node          TEXT NOT NULL,
  count         INTEGER NOT NULL,
  first_ts      INTEGER NOT NULL,
  last_ts       INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS events_last_ts ON events(last_ts);
CREATE INDEX IF NOT EXISTS events_node    ON events(node, last_ts);
CREATE INDEX IF NOT EXISTS events_ns      ON events(namespace, last_ts);

CREATE TABLE IF NOT EXISTS pod_transitions (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  ts        INTEGER NOT NULL,
  namespace TEXT NOT NULL,
  name      TEXT NOT NULL,
  node      TEXT NOT NULL,
  kind      TEXT NOT NULL,   -- phase | restart | waiting
  old       TEXT NOT NULL,
  new       TEXT NOT NULL,
  detail    TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS pod_transitions_ts   ON pod_transitions(ts);
CREATE INDEX IF NOT EXISTS pod_transitions_node ON pod_transitions(node, ts);

-- Current state of every pod, kept for incident open/close decisions.
CREATE TABLE IF NOT EXISTS pods (
  key            TEXT PRIMARY KEY,  -- namespace/name
  namespace      TEXT NOT NULL,
  name           TEXT NOT NULL,
  node           TEXT NOT NULL,
  phase          TEXT NOT NULL,
  waiting_reason TEXT NOT NULL,
  restarts       INTEGER NOT NULL,
  since_ts       INTEGER NOT NULL   -- when the current phase/waiting state began
);

CREATE TABLE IF NOT EXISTS snapshots (
  id                INTEGER PRIMARY KEY AUTOINCREMENT,
  node              TEXT NOT NULL,
  source            TEXT NOT NULL,   -- api | agent
  ts                INTEGER NOT NULL,
  driver            TEXT NOT NULL,
  driver_confidence INTEGER NOT NULL,
  driver_reason     TEXT NOT NULL,
  fields            TEXT NOT NULL,   -- JSON map[string]Field
  baseline          INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS snapshots_node ON snapshots(node, source, ts);

CREATE TABLE IF NOT EXISTS changes (
  id       INTEGER PRIMARY KEY AUTOINCREMENT,
  ts       INTEGER NOT NULL,
  node     TEXT NOT NULL,
  source   TEXT NOT NULL,
  field    TEXT NOT NULL,
  old      TEXT NOT NULL,   -- JSON Field
  new      TEXT NOT NULL,   -- JSON Field
  severity TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS changes_ts   ON changes(ts);
CREATE INDEX IF NOT EXISTS changes_node ON changes(node, ts);

CREATE TABLE IF NOT EXISTS incidents (
  id        INTEGER PRIMARY KEY AUTOINCREMENT,
  opened_ts INTEGER NOT NULL,
  closed_ts INTEGER,
  pods      TEXT NOT NULL    -- JSON list of namespace/name
);
