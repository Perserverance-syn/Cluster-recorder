# Configuration

Every knob, one place. All have working defaults; a bare `helm install` runs. The collector validates
everything at startup and refuses to start with **every** problem listed, not just the first.

## Values

| Helm value | Env var | Default | Type | Restart? | If wrong |
|---|---|---|---|---|---|
| `image.repository` | — | `ghcr.io/perserverance-syn/cluster-recorder` | string | yes | ImagePullBackOff |
| `image.tag` | — | chart `appVersion` | string | yes | ImagePullBackOff |
| `collector.storage.backend` | `STORAGE_BACKEND` | `sqlite` | `sqlite` \| `postgres` | yes | refuses to start. `postgres` is validated but **not implemented** in this release and also refuses to start, with a message saying so |
| `collector.storage.dsn` | `STORAGE_DSN` | `/data/recorder.db` | path | yes | SQLite: file is created; a directory that does not exist fails at open |
| `collector.storage.maxSizeMB` | `STORAGE_MAX_MB` | `4096` | int ≥ 16 | yes | refuses to start. Hard cap on bytes in use; when exceeded, oldest raw snapshots are dropped, then oldest events, with a warning logged. Checked every 5 minutes |
| `collector.persistence.size` | — | `5Gi` | quantity | PVC is immutable | size the PVC above `maxSizeMB` plus WAL headroom; 5Gi for the 4096 default |
| `collector.persistence.storageClass` | — | `""` | string | PVC is immutable | empty = cluster default class; a non-existent class leaves the pod Pending |
| `collector.retention.events` | `RETENTION_EVENTS` | `720h` | Go duration | yes | refuses to start on an unparsable value like `30d` (use `720h`). Also applies to pod transitions |
| `collector.retention.changes` | `RETENTION_CHANGES` | `720h` | Go duration | yes | same. Closed incidents follow this window |
| `collector.retention.snapshots` | `RETENTION_SNAPSHOTS` | `168h` | Go duration | yes | same. Baseline snapshots are never pruned |
| `collector.incident.podThreshold` | `INCIDENT_POD_THRESHOLD` | `3` | int ≥ 1 | yes | refuses to start |
| `collector.incident.namespaceThreshold` | `INCIDENT_NS_THRESHOLD` | `2` | int ≥ 1 | yes | refuses to start |
| `collector.incident.windowMinutes` | `INCIDENT_WINDOW_MIN` | `10` | int minutes ≥ 1 | yes | refuses to start |
| `collector.baseline.stableMinutes` | `BASELINE_STABLE_MIN` | `15` | int minutes ≥ 1 | yes | refuses to start. Also the "all pods Running for N minutes" incident-close rule |
| `collector.logLevel` | `LOG_LEVEL` | `info` | `debug` \| `info` \| `warn` \| `error` | yes | refuses to start |
| `collector.resources` | — | 50m/128Mi requests, 512Mi limit | resources | yes | memory scales with pod count; 512Mi covers a few thousand pods |
| `collector.nodeSelector` | — | `kubernetes.io/os: linux` | map | yes | |
| `collector.tolerations` | — | `[]` | list | yes | |
| `agent.enabled` | — | `false` | bool | yes | opt-in DaemonSet; needs hostNetwork/hostPID, refused on managed clusters |
| `agent.namespace` | — | `recorder-agent` | string | yes | created by the chart with the `privileged` Pod Security label |
| `agent.sampleIntervalSeconds` | `SAMPLE_INTERVAL` | `30` | int ≥ 5 | yes | agent refuses to start |
| `agent.cniDriver` | `CNI_DRIVER` | `auto` | `auto` \| `flannel` \| `calico` \| `cilium` \| `generic` | yes | unknown name is logged and detection runs instead. Force only when detection is provably wrong |
| `agent.bufferSize` | `AGENT_BUFFER` | `100` | int ≥ 1 | yes | samples held in memory while the collector is unreachable; oldest dropped |
| `agent.resources` | — | 20m/32Mi requests, 128Mi limit | resources | yes | |
| `podCIDR` | `POD_CIDR` | `""` | CIDR | yes | override only. Discovery order: `Node.spec.podCIDRs`, `Node.spec.podCIDR`, controller-manager `--cluster-cidr`, then this. If all are empty, `pod_cidr_routes` is `unknown`, never guessed |
| `rbac.calicoIPPools` | — | `false` | bool | yes | adds `get/list` on `ippools.crd.projectcalico.org`. Tier 2 only; harmless otherwise |
| — | `LISTEN_ADDR` | `:8080` | addr | yes | not exposed as a value; the Service targets 8080 |

Namespace is whatever you pass to `helm --namespace`; the plain manifests use `recorder`.

The agent re-runs CNI detection every 10 minutes; a changed answer is recorded as a change on the `driver` field.

Retention runs hourly. Prune reclaims pages (`incremental_vacuum`) and truncates the WAL.

## Worked examples

### Small self-managed cluster

Defaults. Nothing to write.

```bash
helm install recorder ./deploy/helm --namespace recorder --create-namespace
```

### Self-managed cluster with the agent

```bash
helm install recorder ./deploy/helm --namespace recorder --create-namespace --set agent.enabled=true
```

### Managed cluster (EKS / AKS / GKE), collector only

Also defaults. The only thing that varies is the StorageClass, and the default class is used when the
value is empty. If your cluster has no default class:

```bash
helm install recorder ./deploy/helm --namespace recorder --create-namespace \
  --set collector.persistence.storageClass=gp3
```

### Large cluster, longer retention

Postgres is not available in this release. Grow the SQLite cap and the PVC together, and keep raw
snapshots short: they are the bulk, and the diffs are what you read.

```bash
helm install recorder ./deploy/helm --namespace recorder --create-namespace \
  --set collector.storage.maxSizeMB=16384 \
  --set collector.persistence.size=20Gi \
  --set collector.retention.events=1440h \
  --set collector.retention.changes=2160h \
  --set collector.retention.snapshots=72h
```

## Running outside the cluster

The binary falls back to `$KUBECONFIG` or `~/.kube/config` when there is no in-cluster ServiceAccount:

```bash
STORAGE_DSN=./recorder.db LOG_LEVEL=debug go run ./cmd/collector
```