# recorderctl

The operator's view of the recorder. It reads the same API the Headlamp plugin will, prints plain tables, and
never writes anything.

## Install

Pick one.

**Go toolchain** (Go 1.27+):

```bash
go install github.com/Perserverance-syn/Cluster-recorder/cmd/recorderctl@latest
```

**Release binary**: every tagged release attaches `recorderctl-linux-amd64`, `recorderctl-linux-arm64`,
`recorderctl-darwin-arm64` and `recorderctl-windows-amd64.exe`. Download the one for your machine, then:

```bash
chmod +x recorderctl-linux-amd64 && sudo mv recorderctl-linux-amd64 /usr/local/bin/recorderctl
```

**From a clone**:

```bash
go build -o recorderctl ./cmd/recorderctl
```

## Prerequisites

`kubectl` on the PATH with a kubeconfig that can `port-forward` to the collector's Service. That is the only
cluster permission it uses. If `RECORDER_URL` already answers (you opened a port-forward yourself, or you are
inside the cluster), no `kubectl` is needed at all.

## Commands

| Command | Answers |
|---|---|
| `recorderctl status` | Is anything wrong right now? One table (per node: last snapshots, driver, baseline, fields that differ) and a verdict: `OK`, or an `ATTENTION` list of nodes that differ from baseline, silent agents, and open incidents. |
| `recorderctl diff [node ...]` | Every field that differs from the last healthy baseline, per source (`api`, `agent`). No node means all nodes. `matches baseline` when nothing differs. |
| `recorderctl timeline <node> [-since 6h]` | Everything recorded on that node, oldest first: change records with severity, events with counts, pod transitions. |
| `recorderctl incidents [-all]` | Open incidents (or all from the last 30 days with `-all`), each with every change record cluster-wide from the six hours before it opened. |
| `recorderctl events [-since 1h] [-node n] [-namespace ns]` | Retained events, past the API server's TTL. |

`-since` takes a Go duration: `30m`, `6h`, `48h`.

## Reading the output

- `ABSENT` in a change or diff means the read succeeded and the thing is gone. `present -> ABSENT` is an `ALERT`.
- `n/a` means the CNI has no such concept by design. It never changes and never alarms.
- `unknown (...)` means the sample failed, with the reason. It is a recorder or permissions problem, not a cluster
  problem, and it never produces an alert.
- `status` shows `-` for a baseline when the cluster has not yet been quiet for the stable window (15 minutes by
  default: all nodes Ready, no crashloops, no restarts, no alerts). Until then `diff` has nothing to compare against.

## Flags

| Flag | Env var | Default | Meaning |
|---|---|---|---|
| `-url` | `RECORDER_URL` | `http://localhost:8080` | collector URL; when it does not answer, a port-forward is opened |
| `-n` | `RECORDER_NAMESPACE` | `recorder` | namespace of the collector Service |
| `-svc` | `RECORDER_SERVICE` | `recorder` | Service name, which is the Helm release name |

## Typical investigation

```bash
recorderctl status                       # ATTENTION: worker-2 differs from baseline: pod_bridge.ipv4, ...
recorderctl diff worker-2                # pod_bridge.ipv4  10.244.2.1 -> ABSENT
recorderctl timeline worker-2 -since 6h  # find the minute it vanished and what preceded it
recorderctl incidents                    # which pods it took down, and every change in the 6h before
```