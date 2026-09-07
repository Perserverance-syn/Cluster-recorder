# Upgrading

## Version skew

The chart `version` and image `appVersion` move together. Do not mix a newer image with an older chart:
environment variables are the contract between them and new ones have defaults, removed ones are ignored,
but renamed ones are a breaking change and listed here.

Kubernetes: n-3 minor versions. Only stable API groups are used (`v1`, `apps/v1`, `rbac.authorization.k8s.io/v1`,
`events.k8s.io/v1`). No CRDs of its own.

## Schema migrations

The store applies `internal/store/migrations/*.sql` in order on startup and records the count in
`PRAGMA user_version`. Migrations are additive; a downgrade is not supported. Back up the PVC before a major
version bump:

```bash
kubectl -n recorder scale deploy/recorder --replicas=0
kubectl -n recorder run backup --image=busybox:1.36 --restart=Never --overrides='{"spec":{"containers":[{"name":"b","image":"busybox:1.36","command":["sh","-c","cat /data/recorder.db"],"volumeMounts":[{"name":"d","mountPath":"/data"}]}],"volumes":[{"name":"d","persistentVolumeClaim":{"claimName":"recorder-data"}}]}}'
kubectl -n recorder wait --for=condition=Ready pod/backup --timeout=60s
kubectl -n recorder exec backup -- cat /data/recorder.db > recorder-backup.db
kubectl -n recorder delete pod backup
kubectl -n recorder scale deploy/recorder --replicas=1
```

## Upgrade procedure

```bash
helm upgrade recorder ./deploy/helm --namespace recorder --reuse-values
kubectl -n recorder rollout status deploy/recorder
curl -s localhost:8080/api/v1/readyz
```

`Recreate` strategy: there is a gap of a few seconds with no watch. Events raised in that gap are still
picked up by the initial list on restart, because the API server keeps them for an hour.

## Breaking changes

None yet. This section lists renamed values and env vars per version.