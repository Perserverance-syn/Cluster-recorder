# Architecture

## Components

```
                       Kubernetes API server
                               │  watch (informers, resync 5m)
                               ▼
┌──────────────── recorder-collector (Deployment, 1 replica) ────────────────┐
│  internal/informers   Events → store            Pods → transitions only    │
│                       Nodes → NodeSnapshot → diff vs previous → changes    │
│  internal/incident    every 30s: group CrashLoopBackOff pods; mark baselines│
│  internal/store       SQLite on the PVC; retention hourly; size cap 5-minly│
│  internal/api         GET everything; POST /agent/snapshots (tier 2 ingest)│
└─────────────────────────────────────────────────────────────────────────────┘
                               ▲
                               │  POST snapshots every 30s; GET hints (DaemonSets, pod CIDRs)
                  recorder-agent DaemonSet (tier 2, hostNetwork, netlink)
```

Each package owns one thing:

| Package | Owns | Never does |
|---|---|---|
| `internal/sample` | `Field`, `FieldState`, `Snapshot` | anything with a database or the API server |
| `internal/diff` | the transition table | I/O |
| `internal/store` | all SQL | policy (what to prune is a parameter) |
| `internal/informers` | client-go, conversion of API objects to records | HTTP |
| `internal/incident` | grouping and baseline rules, as a periodic pass over the store | event handling |
| `internal/api` | HTTP, time parsing, JSON | SQL |
| `internal/config` | env parsing, defaults, validation | reading files |

## Data flow: from API server to API response

1. An informer delivers a `Node` update. `informers.NodeSnapshot` maps `Node.status` into `node.*` fields.
2. The previous `api` snapshot for that node is loaded. If the field maps are identical (a resync), nothing is written.
3. Otherwise the snapshot is stored and `diff.Snapshots(prev, cur)` produces change records via the transition table.
4. Change records, events (every one, on add and update) and pod transitions (phase, restart, waiting-reason) land in their tables with the node name attached. For pod events, the node comes from the pod lister.
5. `GET /api/v1/nodes/{node}/timeline` reads the three tables for that node and time range and merges them by time.

Events are written on every add and update, and deletes are ignored. The delete *is* the TTL expiry.

## Storage schema

SQLite, WAL mode, one connection. Timestamps are integer Unix milliseconds. `internal/store/migrations/001_init.sql`
is the source of truth. Tables:

| Table | Row is | Keyed by | Pruned by |
|---|---|---|---|
| `events` | one Kubernetes event, last-write-wins on count | event UID | `retention.events` |
| `pod_transitions` | one phase / restart / waiting change | autoincrement | `retention.events` |
| `pods` | current state of one pod, with `since_ts` for the current state | `ns/name` | never (deleted on pod delete) |
| `snapshots` | one node's full field map from one source | autoincrement | `retention.snapshots`, except `baseline=1` |
| `changes` | one field transition | autoincrement | `retention.changes` |
| `incidents` | opened, closed, affected pods | autoincrement | `retention.changes`, once closed |

The size cap counts pages in use. When exceeded, the oldest non-baseline snapshots go in batches of 100, then
the oldest events. It is checked every 5 minutes and the WAL is truncated hourly.

## Why four field states

Every sampled field is `present`, `absent`, `not_applicable` or `unknown`. Each pair that could be collapsed
produces a specific false alarm:

| If you collapsed… | You would get |
|---|---|
| `absent` into `not_applicable` | Flannel losing `flannel.1` looks like Calico-in-routing-mode. The outage is invisible. |
| `not_applicable` into `absent` | A healthy Calico cluster alarms forever on a bridge that never existed. |
| `unknown` into `absent` | A node where netlink is unreadable looks like a node whose bridge vanished. One permission error becomes a flood of alerts, and the tool gets ignored. |
| `unknown` into `present` (zero value) | A read failure looks like a healthy read. The outage is invisible. |

The transition table in `internal/diff` is the whole engine:

| Transition | Emits | Severity |
|---|---|---|
| `present → absent` | yes | `alert` |
| `present → present`, value changed | yes | `info` |
| `absent → present` | yes | `recovery` |
| anything `→ unknown` | yes | `degraded` (sampling problem, not cluster problem) |
| `unknown → anything` | **no** | you cannot claim something vanished that you never saw |
| anything `↔ not_applicable` | **no** | |

`unknown → absent` emitting nothing is deliberate and tested. It waits for a clean read.

A `Field` is always marshalled as `{state, value?, reason?}`. No layer flattens it to a bare value.

## Field namespaces

- `node.*` — collector-side, from `Node.status`, CNI-independent: `node.ready`, `node.memory_pressure`,
  `node.disk_pressure`, `node.pid_pressure`, `node.network_unavailable`, `node.unschedulable`, `node.boot_id`
  (changes on reboot), `node.kernel_version`, `node.os_image`, `node.container_runtime`, `node.kubelet_version`,
  `node.pod_cidrs`, `node.internal_ip`, `node.taints`.
- Agent fields (`pod_bridge.*`, `overlay_tunnel.*`, `pod_cidr_routes`, …) arrive through `POST /api/v1/agent/snapshots`
  with `source=agent` and are diffed against the previous agent snapshot for that node. A change of `driver`
  between agent snapshots is itself recorded.

## Why informers, not polling

Polling misses every transition that happens between polls, and a poll per API request does not scale past a
few users. Shared informers give the collector a local cache and every transition, with one watch per resource.
The 5-minute resync is a safety net; unchanged resyncs write nothing.

## Incidents and baselines

Every 30 seconds:

- **Open** when at least `podThreshold` pods across at least `namespaceThreshold` namespaces entered
  `CrashLoopBackOff` within `windowMinutes`. The incident starts at the earliest of those transitions.
  Later crashloops join the open incident.
- **Close** when every affected pod has been `Running` with no waiting reason for `stableMinutes`, or is gone.
- **Baseline** the latest snapshot of every node when the cluster has been quiet for `stableMinutes`: all nodes
  `Ready` with no readiness flap, nothing in `CrashLoopBackOff`, no restarts. The newest four baselines per
  node and source are kept and never pruned. `GET /nodes/{node}/diff` compares against the newest.

## What is intentionally not stored

- Every pod update. Only phase, restart and waiting-reason transitions.
- Metrics. Prometheus.
- Logs. Loki.
- Secrets, ConfigMaps, or any resource body.
- Anything from a namespace or node you filtered out: nothing, because there is no filter. Cluster-wide is the point.