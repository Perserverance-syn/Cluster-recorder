# Limits

A precise scope statement, not a modest disclaimer.

## What this release catches

- Any Kubernetes event, kept for 30 days instead of 1 hour.
- Pod phase changes, container restarts with the terminating reason and exit code, container waiting-reason
  changes (`CrashLoopBackOff`, `ImagePullBackOff`, …). Not every pod update.
- Node condition flips (`Ready`, pressure conditions), reboots (`node.boot_id` changes), kubelet or runtime
  version changes, taint changes, pod CIDR changes, internal IP changes.
- Clusters of failure: N pods across M namespaces crashlooping in a window, with every change record from the
  preceding six hours attached.
- What differs now from the last time the cluster was quiet, field by field.

## What it structurally cannot catch

- **Anything without a leading edge.** A node that reboots hard gives no warning. You get the `boot_id` change
  after the fact, which answers "did it reboot?" and not "why".
- **Kernel network state without the agent.** Bridge addresses, VXLAN tunnels, routes, neighbour tables need tier 2.
  Without it the API omits the `agent` source rather than reporting anything wrong.
- **Restart counts.** The agent reports kubelet and containerd *start times* (a change is a restart), not a count:
  a count needs systemd D-Bus, which the agent does not talk to.
- **Flannel backend, Calico mode and Cilium mode are inferred from interfaces**, not read from the CNI's own config,
  and pinned for the agent's lifetime. A CNI reconfigured live needs an agent restart to change its topology.
- **Anything between informer deliveries.** A pod that goes `Running → CrashLoopBackOff → Running` inside one
  informer update is seen as unchanged.
- **Causes.** It records what changed and when. Diagnosis is yours.

## What it deliberately leaves to other tools

- Metrics and time series: Prometheus. There is no numeric field history here beyond restart counts.
- Logs: Loki.
- Resource CRUD, exec, port-forward: Headlamp or kubectl.
- Alert delivery: nothing here notifies anyone. Phase 2 rules over change records will emit to whatever you run.
- Auto-remediation: never, in any version, without a blast-radius limit, rate limiter, audit log and kill switch.

## Boundaries of this release

- **One cluster per deployment.** No aggregation. The API is versioned (`/api/v1/`) so this can change.
- **No auth on the API.** ClusterIP only. Exposing it is a release blocker until auth exists.
- **SQLite only.** `STORAGE_BACKEND=postgres` is accepted by validation and refuses to start.
- **Single replica.** No leader election.
- **IPv6 is recorded, not treated specially.** Untested against a v6-only cluster.
- **Linux only.** Windows nodes are watched through the API like any other; a future agent will skip them.
- **No repair recipes yet.** The data model is designed for them; none ship.
- **Drivers shipped: flannel, calico, cilium, generic.** Calico and Cilium are tested on synthetic fixtures, not a
  live cluster. Anything else runs generic: full data, neutral names, bridge and overlay `not_applicable`.
