# Changelog

Semver. Human-written.

## Unreleased

### Added
- Tier-1 collector: event retention past TTL, pod transition history, node condition snapshots, diff engine, incident grouping, healthy baselines, read-only API.
- SQLite storage with retention windows and a hard size cap.
- Helm chart and generated plain manifests. Runs under Pod Security `restricted`.
- Agent snapshot ingest endpoint (`POST /api/v1/agent/snapshots`) and detection hints (`GET /api/v1/agent/hints`).
- Tier-2 node agent: netlink sampling with no capabilities, CNI abstraction with flannel, calico, cilium and generic
  drivers, host facts (boot time, kubelet/containerd start times, memory/disk/inode pressure), in-memory buffer.
- `hack/nldump` to capture driver fixtures from real nodes.

### Not yet
- Postgres backend. `STORAGE_BACKEND=postgres` refuses to start.
- Repair recipes.