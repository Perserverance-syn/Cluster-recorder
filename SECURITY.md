# Security

## Threat model

The collector holds a read-only, cluster-wide view: pod names, namespaces, node IPs, event messages,
node kernel/OS versions. That is reconnaissance material. It is served unauthenticated on a ClusterIP.

**What the collector can do:** `get/list/watch` on events, pods, nodes, namespaces, DaemonSets. Nothing else.
It cannot create, update, patch, delete, exec, port-forward, or read Secrets. See [docs/RBAC.md](docs/RBAC.md).

**What an attacker with network access to the API learns:** everything above, for the retention window (30 days
by default). They cannot change anything through it: every endpoint but one is `GET`, and the one `POST`
(`/api/v1/agent/snapshots`) only inserts a node snapshot, which could pollute the timeline for that node.

**What an attacker with the collector's ServiceAccount token can do:** read the same things directly from the
API server. No more.

## Rules that follow

- Never expose the Service outside the cluster in this version. Adding auth is a release blocker before any
  external exposure.
- Restrict access to the `recorder` namespace with a NetworkPolicy if your CNI enforces them.
- The PVC contains the same data; treat it like you treat event logs.

## Reporting

Open a private security advisory on the repository, or email the maintainers listed in the repository
metadata. Please do not file public issues for vulnerabilities.