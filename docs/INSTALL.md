# Install

Written against a fresh clone. If a step only works because of a specific kubeconfig, CNI or StorageClass,
that is a bug in this doc: please file it.

## What must exist first

| Requirement | Tier 1 (collector) | Tier 2 (+ agent) |
|---|---|---|
| Kubernetes | 1.27 or newer (n-3 policy) | same |
| Access to install | create a namespace, a ClusterRole and a ClusterRoleBinding | plus a second namespace |
| ServiceAccount | 1, read-only cluster-wide | 2; the agent's has **no** API access and no token |
| Storage | one PVC, 5Gi by default, any StorageClass with `ReadWriteOnce` | same |
| Pod Security Admission | runs under `restricted` | agent namespace is labelled `privileged` by the chart |
| hostNetwork / hostPID | not needed | required; `privileged: false`, all capabilities dropped |
| Node access | none | DaemonSet on every node, control plane included |
| Managed clusters (EKS, AKS, GKE) | supported | usually not permitted |
| Network egress | none | none |
| External dependencies | none (SQLite on the PVC) | none |
| CNI | any | Flannel, Calico, Cilium detected; anything else runs the generic driver |

## What permissions you are granting

Read-only. `get`, `list`, `watch` on events, pods, nodes, namespaces and DaemonSets. **Zero write verbs**,
no `pods/exec`, no `pods/portforward`, no `secrets`. The exact ClusterRole, verb by verb with justification,
is in [RBAC.md](RBAC.md). Hand that file to your security review.

## Which install path

```
Do you control the nodes (self-managed / kubeadm / on-prem)?
├── No  (EKS / AKS / GKE / restricted PSA)
│      → Tier 1:  helm install recorder ./deploy/helm --namespace recorder --create-namespace
│        Events, pod transitions, node conditions, incidents, baselines.
│
└── Yes
       → Tier 1 + 2:  add --set agent.enabled=true
         Adds pod bridge, overlay tunnel, routes, neighbours, boot time, kubelet
         and runtime state per node. Verify the detected driver before trusting it.
```

### Helm

```bash
helm install recorder ./deploy/helm --namespace recorder --create-namespace
```

No values file needed. To change anything, see [CONFIGURATION.md](CONFIGURATION.md).

With the agent, on nodes you control:

```bash
helm install recorder ./deploy/helm --namespace recorder --create-namespace --set agent.enabled=true
```

Already installed? Use `helm upgrade ... --reset-then-reuse-values --set agent.enabled=true`. Plain `--reuse-values`
drops the `agent.namespace` default and the upgrade fails on an empty Namespace name.

### Without Helm

```bash
kubectl apply -f deploy/manifests/
```

The manifests are generated from the chart with default values and CI fails if they drift. Namespace `recorder`,
release name `recorder`.

## Verify

```bash
kubectl -n recorder get pods
kubectl -n recorder port-forward svc/recorder 8080:8080 &

curl -s localhost:8080/api/v1/healthz
curl -s localhost:8080/api/v1/readyz
curl -s "localhost:8080/api/v1/events?from=-10m" | head
curl -s "localhost:8080/api/v1/nodes/$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')/timeline?from=-1h"
```

Expected:

- `healthz` returns `{"status":"ok"}`; `readyz` returns `{"status":"ready"}` once the informer caches have synced
  (seconds on small clusters, up to a minute on large ones).
- `events` returns a non-empty list within a minute on any cluster that is doing anything. On a completely idle
  cluster, trigger one: `kubectl run verify --image=busybox:1.36 --restart=Never -- sh -c 'exit 1'`, then delete it.
- `timeline` returns a JSON array. It contains node condition fields (`node.ready`, `node.boot_id`, …) as
  `change` entries once anything on the node changes, and events and pod transitions for pods on that node.

With the agent enabled, also:

```bash
kubectl -n recorder-agent get pods -o wide
curl -s localhost:8080/api/v1/nodes
```

Every node must show `last_agent_snapshot` within one sample interval (30s) and a `driver`. **If the driver is
`generic` on a Flannel, Calico or Cilium cluster, detection failed: file it as a bug, do not assume it is fine.** The
`diff` endpoint then carries an `agent` source with `pod_bridge`, `overlay_tunnel`, `pod_cidr_routes` and the rest.

If `readyz` stays at 503, see [TROUBLESHOOTING.md](TROUBLESHOOTING.md).

For day-to-day use, install the CLI instead of curling: [RECORDERCTL.md](RECORDERCTL.md). `recorderctl status` does the
whole verification above in one command.

## Uninstall

```bash
helm uninstall recorder --namespace recorder
kubectl -n recorder delete pvc recorder-data     # the data; Helm leaves PVCs behind on purpose
kubectl delete namespace recorder                # recorder-agent is removed with the release
```

Without Helm:

```bash
kubectl delete -f deploy/manifests/
kubectl -n recorder delete pvc recorder-data
```

The ClusterRole and ClusterRoleBinding are named `recorder-collector` and are removed by either path.