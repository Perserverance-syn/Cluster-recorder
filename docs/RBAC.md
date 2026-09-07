# RBAC

Everything the recorder is allowed to do, verb by verb, with the reason. Hand this file to the security review.

## The one sentence that matters

**Zero write verbs. No `create`, `update`, `patch`, `delete`, no `pods/exec`, no `pods/portforward`, no `secrets`,
anywhere.** The recorder is provably incapable of changing the cluster.

## Collector ClusterRole (shipped verbatim in `deploy/helm/templates/rbac.yaml`)

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: recorder-collector
rules:
  # Core forensic data. Events are the whole point: Kubernetes expires them in 1h.
  - apiGroups: [""]
    resources: ["events", "pods", "nodes", "namespaces"]
    verbs: ["get", "list", "watch"]

  # events.k8s.io mirrors core events on newer clusters.
  - apiGroups: ["events.k8s.io"]
    resources: ["events"]
    verbs: ["get", "list", "watch"]

  # CNI detection hints (tier 2): which network DaemonSets exist cluster-wide.
  # The collector reads this so the AGENT needs no API access at all.
  - apiGroups: ["apps"]
    resources: ["daemonsets"]
    verbs: ["get", "list", "watch"]
```

| Resource | Verbs | Why |
|---|---|---|
| `events` (core and `events.k8s.io`) | get, list, watch | The product. Retained past the 1h TTL. |
| `pods` | get, list, watch | Phase, restart and waiting transitions; mapping pod events to their node. |
| `nodes` | get, list, watch | Conditions, boot ID, versions, pod CIDRs: the `node.*` fields. |
| `namespaces` | get, list, watch | Reserved for namespace metadata on incidents; harmless to grant now, avoids a chart bump later. |
| `daemonsets` (`apps`) | get, list, watch | Tier-2 CNI detection hints. Granted now so the agent, when it ships, never needs its own API access. |

Optional, only with `rbac.calicoIPPools=true`, for Calico overlay-mode detection in tier 2:

```yaml
  - apiGroups: ["crd.projectcalico.org"]
    resources: ["ippools"]
    verbs: ["get", "list"]
```

Without it, detection degrades to inspecting which interfaces exist. It never fails.

## Agent (tier 2)

The agent runs with a ServiceAccount that has **no Role, no ClusterRole, no binding** and
`automountServiceAccountToken: false`. It reads the local node via netlink and POSTs to the collector; the cluster
facts it needs (DaemonSets for detection, pod CIDRs) come from the collector over `GET /api/v1/agent/hints`.
Compromising it yields nothing against the API server. Shipped verbatim in `deploy/helm/templates/agent.yaml`:

```yaml
hostNetwork: true      # netlink must read the HOST namespace, not the pod's
hostPID: true          # /proc/<pid> of kubelet and containerd for restart detection
securityContext:
  runAsUser: 0         # netlink socket and /etc/cni/net.d are root-readable only
  privileged: false    # NOT privileged: read-only netlink needs no capabilities
  capabilities:
    drop: ["ALL"]
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
tolerations:
  - operator: Exists   # control-plane nodes are affected too, and always skipped
```

`privileged: false` with all capabilities dropped is the difference from most node agents, and it is what gets the
DaemonSet past review. Mounts: `/etc/cni/net.d` and `/` at `/host`, both read-only, the latter for `statfs` only.

## What an attacker with collector API access learns

Pod names, namespaces, node names and internal IPs, kernel and kubelet versions, event messages, and the
timeline of failures for the retention window. Not Secrets, not ConfigMaps, not logs, not resource bodies.
They can insert agent snapshots for a node, which pollutes that node's timeline; they cannot delete or alter
anything else. See [../SECURITY.md](../SECURITY.md).

## What the collector can reach

The API server (read-only, above) and its own PVC. It makes no other network connections. The container runs
as UID 65532, non-root, read-only root filesystem, all capabilities dropped, `RuntimeDefault` seccomp, and
passes Pod Security Admission `restricted`.

## Leader election

Not granted. v1 is single-replica. When multi-replica arrives it will need a **namespaced** Role for
`leases.coordination.k8s.io` (`get`, `create`, `update`), the only write verbs this project will ever hold.