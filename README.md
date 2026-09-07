# Cluster Recorder

Records Kubernetes **and node-level** state, keeps it past Kubernetes' own TTLs, and answers:
*what changed on this node in the hours before things broke, and how does that differ from when it was healthy?*

Kubernetes expires events after one hour. The evidence for most outages is gone before anyone looks.
This tool keeps it, turns node state into a field-by-field timeline, and groups pod failures into incidents.

## Why this exists

Kubernetes events expire after one hour. By the time someone investigates an outage, the events that explain what
triggered it are usually gone, and the investigation starts from the symptoms instead of the cause.

A whole class of failures never reaches the API at all. A CNI bridge that loses its address, an overlay tunnel that
disappears, peer routes that drop: pods crashloop, the node still reports Ready, and every dashboard that watches
the API server shows a healthy cluster with unhealthy pods. That state lives in the node's kernel, and no
standard tool records it as a timeline next to pod health.

This recorder keeps events past their TTL, records pod transitions and node-level network state field by field,
diffs each sample against the last one, and keeps a healthy baseline to compare against. The question it answers
is the one that matters at 3am: *what changed on this node before things broke?*

## What it does not do

- It does **not** replace Prometheus (metrics) or Loki (logs). It records *state transitions*, not time series.
- It does **not** auto-remediate. Nothing here writes to the cluster: the ServiceAccount has zero write verbs.
- The node agent (tier 2: kernel network state via netlink) needs `hostNetwork` and `hostPID`. It is opt-in and will not run on managed clusters that forbid those.
- No authentication on the API. ClusterIP-only. Do not expose it. See [docs/LIMITS.md](docs/LIMITS.md).

## 60-second quickstart

Any conformant cluster, Kubernetes 1.27+, no values file:

```bash
helm install recorder ./deploy/helm --namespace recorder --create-namespace
kubectl -n recorder port-forward svc/recorder 8080:8080 &
curl -s localhost:8080/api/v1/healthz
curl -s "localhost:8080/api/v1/events?from=-10m" | head
curl -s "localhost:8080/api/v1/nodes/$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')/timeline?from=-1h"
```

No Helm? `kubectl apply -f deploy/manifests/` installs the same thing (generated from the chart, verified in CI).

On nodes you control, add the agent. It detects Flannel, Calico or Cilium and falls back to a generic driver on anything else:

```bash
helm upgrade recorder ./deploy/helm --namespace recorder --reset-then-reuse-values --set agent.enabled=true
```

## Checking the cluster with `recorderctl`

A small CLI that talks to the collector, opens its own `kubectl port-forward` when needed, and prints plain tables.

Install one of three ways:

```bash
# from the Go toolchain
go install github.com/Perserverance-syn/Cluster-recorder/cmd/recorderctl@latest
```

```bash
# from a release: pick the binary for your OS from the Releases page, then
chmod +x recorderctl && sudo mv recorderctl /usr/local/bin/
```

```bash
# from a clone
go build -o recorderctl ./cmd/recorderctl
```

It needs `kubectl` on the PATH with a working kubeconfig, nothing else. Then:

```bash
recorderctl status                    # one-line verdict: OK, or what differs from baseline and which incidents are open
recorderctl diff worker-2             # every field on that node that differs from the last healthy baseline
recorderctl timeline worker-2 -since 6h   # what happened on that node, oldest first
recorderctl incidents                 # open incidents, each with every change from the 6 hours before it
recorderctl incidents -all            # include closed ones from the last 30 days
recorderctl events -since 30m -namespace payments
```

Flags `-url`, `-n` and `-svc` (or `RECORDER_URL`, `RECORDER_NAMESPACE`, `RECORDER_SERVICE`) point it at a
non-default install. Full reference: [docs/RECORDERCTL.md](docs/RECORDERCTL.md).

## What the output looks like

One node's timeline, oldest first. Change records name the field, the old and new state, and a severity:

```json
[
  {"time":"2026-09-06T12:46:31Z","kind":"change","node":"worker-2","data":{"field":"node.boot_id","old":{"state":"present","value":"4f1c…"},"new":{"state":"present","value":"9a72…"},"severity":"info"}},
  {"time":"2026-09-06T12:47:02Z","kind":"change","node":"worker-2","data":{"field":"node.ready","old":{"state":"present","value":"True"},"new":{"state":"present","value":"False"},"severity":"info"}},
  {"time":"2026-09-06T12:48:10Z","kind":"pod_transition","node":"worker-2","data":{"namespace":"payments","name":"api-7d9f","kind":"waiting","old":"","new":"CrashLoopBackOff","detail":"container=api"}},
  {"time":"2026-09-06T12:48:11Z","kind":"event","node":"worker-2","data":{"reason":"BackOff","message":"Back-off restarting failed container","count":3}}
]
```

Every field carries one of four states: `present`, `absent`, `not_applicable`, `unknown`. The difference between
"the overlay vanished" and "this CNI has no overlay" and "we could not read it" is the whole design.
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) explains why.

## Where next

| You want to… | Read |
|---|---|
| Install it properly, uninstall it cleanly | [docs/INSTALL.md](docs/INSTALL.md) |
| Know exactly what permissions you are granting | [docs/RBAC.md](docs/RBAC.md) |
| Tune retention, size cap, incident thresholds | [docs/CONFIGURATION.md](docs/CONFIGURATION.md) |
| Understand the components and the data model | [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) |
| Use the CLI | [docs/RECORDERCTL.md](docs/RECORDERCTL.md) |
| Call the API | [docs/API.md](docs/API.md) |
| Fix the recorder itself | [docs/TROUBLESHOOTING.md](docs/TROUBLESHOOTING.md) |
| Know what it will never catch | [docs/LIMITS.md](docs/LIMITS.md) |
| Add a CNI driver | [docs/ADDING_A_CNI_DRIVER.md](docs/ADDING_A_CNI_DRIVER.md) |
| Contribute | [CONTRIBUTING.md](CONTRIBUTING.md) |

## Development

```bash
make test        # unit + fake-API-server tests; no cluster, no root, no network beyond modules
make kind-up && make e2e && make kind-down
```

Go 1.27+. Licensed under [Apache 2.0](LICENSE).