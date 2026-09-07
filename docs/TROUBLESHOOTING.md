# Troubleshooting the recorder

Symptom first. Each entry: the diagnostic command, then the fix.

## Collector pod CrashLoops on start

```bash
kubectl -n recorder logs deploy/recorder --previous | head -20
```

- `fatal: configuration invalid:` followed by a list: every bad value is listed at once. Fix them all, `helm upgrade`.
  Common ones: `RETENTION_EVENTS: "30d" is not a duration` (use `720h`); `STORAGE_BACKEND=postgres is not implemented`.
- `fatal: open storage /data/recorder.db: ... unable to open database file`: the PVC is not mounted or not
  writable by UID 65532. `kubectl -n recorder describe pod -l app.kubernetes.io/name=cluster-recorder`
  and look at the volume events; check the StorageClass supports `fsGroup`.
- `no in-cluster config and no kubeconfig`: the ServiceAccount token is not mounted. The chart sets
  `automountServiceAccountToken: true`; a cluster-wide admission policy may be stripping it.

## Pod stays Pending

```bash
kubectl -n recorder get pvc recorder-data
kubectl -n recorder describe pvc recorder-data
```

`Pending` PVC: no default StorageClass, or the named one does not exist. Set `collector.persistence.storageClass`.

## `readyz` returns 503 forever

```bash
kubectl -n recorder logs deploy/recorder | grep -i -E "forbidden|sync"
```

`forbidden` means the ClusterRoleBinding is missing or points at the wrong ServiceAccount:

```bash
kubectl auth can-i list events --as=system:serviceaccount:recorder:recorder
kubectl auth can-i watch nodes --as=system:serviceaccount:recorder:recorder
```

Both must say `yes`. If not, `kubectl get clusterrolebinding recorder-collector -o yaml` and check `subjects`.
On very large clusters the initial list can take over a minute; the readiness probe tolerates that.

## Timeline is empty

```bash
NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
curl -s "localhost:8080/api/v1/nodes/$NODE/snapshots?from=-24h" | head -c 400
curl -s "localhost:8080/api/v1/nodes" 
```

- `/nodes` lists nothing: the node informer has not delivered. See the 503 entry above.
- Snapshots exist but the timeline is empty: nothing has changed on that node in the window. The timeline
  holds *changes*, not samples. Widen `from`, or look at `/diff` for the current state.
- Node name mismatch: the path takes the Kubernetes node name exactly (`kubectl get nodes`), which is not
  always the hostname.

## Events stop appearing

```bash
kubectl -n recorder logs deploy/recorder | grep -i "store event"
```

Errors here are storage errors. Check the size cap entry below and the PVC's free space.

## Database hit its size cap

```bash
kubectl -n recorder logs deploy/recorder | grep "over size cap"
```

Expected behaviour: the log warns, the oldest raw snapshots are dropped, then the oldest events, until under
`collector.storage.maxSizeMB`. The recorder keeps running. If it fires constantly, raise `maxSizeMB` (and the
PVC) or shorten `retention.snapshots`. If it logs `nothing left to drop`, the cap is below the size of the
baselines plus indexes: raise it.

## PVC is full but the cap was not reached

WAL headroom. SQLite's WAL can briefly hold a second copy of recent writes; the PVC should be at least the cap
plus 25%. The default pairing (4096 MB cap, 5Gi PVC) respects this. The WAL is truncated hourly.

## Incident never opens during an obvious outage

```bash
curl -s "localhost:8080/api/v1/events?from=-1h" | grep -c CrashLoop
```

The rule is `≥ podThreshold` pods across `≥ namespaceThreshold` namespaces within `windowMinutes`. Three pods
in one namespace does not open one by default. Lower `namespaceThreshold` to 1 if that is what you want.

## Incident never closes

Every affected pod must be `Running` with no waiting reason for `stableMinutes`, or be deleted. A pod stuck in
`Completed` or `Pending` holds it open. Delete the pod or the incident closes when it does.

## No baseline in `/diff`

Baselines need `stableMinutes` of: all nodes `Ready` with no flap, no `CrashLoopBackOff`, no restarts. A cluster
with one perpetually crashlooping pod never baselines. Fix or delete the pod.

## Agent pods blocked by Pod Security Admission

```bash
kubectl -n recorder-agent get events --field-selector reason=FailedCreate
```

The chart labels `recorder-agent` with `pod-security.kubernetes.io/enforce: privileged`. A cluster-wide admission
policy (Kyverno, Gatekeeper, a managed control plane) may still refuse `hostNetwork`/`hostPID`. On managed
clusters, run tier 1 only. Never move the agent into the collector namespace: that namespace stays `restricted`.

## No agent fields appear

```bash
kubectl -n recorder-agent logs ds/recorder-agent --tail=20
curl -s localhost:8080/api/v1/nodes
```

- `collector unreachable, buffering`: the agent cannot reach `recorder.recorder.svc:8080`. It keeps up to
  `agent.bufferSize` samples and flushes them in order when the collector is back. Persistent: check the Service and
  that hostNetwork DNS resolves cluster names (`dnsPolicy: ClusterFirstWithHostNet` is set by the chart).
- `last_agent_snapshot` missing for one node: that node's pod is not running. `kubectl -n recorder-agent get pods -o wide`.
- All fields `unknown` with `netlink:` in the reason: the pod is not on the host network or a policy stripped
  `hostNetwork`. Inspect the pod spec.

## Wrong CNI detected / driver is `generic` on Flannel, Calico or Cilium

```bash
kubectl -n recorder-agent logs ds/recorder-agent | grep 'cni driver selected'
```

The reason lists which signals matched. Detection reads `/etc/cni/net.d` (mounted read-only), DaemonSet names from
the collector, and interface names. If your CNI config lives elsewhere, mount it at `/etc/cni/net.d` or pin the
driver with `agent.cniDriver`. Then **file a bug with the reason string**: this is a defect, not a state to accept.

## `pod_cidr_routes` is `unknown`

Pod CIDR discovery found nothing: no `Node.spec.podCIDRs` (common with Calico IPAM and cloud CNIs) and no
readable `kube-controller-manager --cluster-cidr`. Set `podCIDR` in values. It is never guessed.
