# Contributing

## Dev setup

Go 1.27+. Nothing else for unit tests.

```bash
git clone <this repo> && cd cluster-recorder
make test
```

For end-to-end: docker, kind, helm, kubectl, jq.

```bash
make kind-up && make e2e && make kind-down
```

To run the collector from a laptop against any cluster you have a kubeconfig for:

```bash
STORAGE_DSN=./recorder.db go run ./cmd/collector
```

## PR expectations

- `make fmt-check vet test helm-lint manifests-check docs-check` passes. CI runs exactly that.
- A change to `deploy/helm/values.yaml` ships with a change to `docs/CONFIGURATION.md` in the same PR. CI enforces it.
- `docs/API.md` is generated: edit `internal/api/openapi.yaml`, run `make docs`.
- `deploy/manifests/` is generated: edit the chart, run `make manifests`.
- New logic gets one test that fails if the logic breaks. The diff transition table and the
  `unknown -> absent` row in particular must stay covered.
- No write verbs in RBAC. No new external dependencies without a reason in the PR.
- Commit messages: imperative subject, body says why.

## Running a single package

```bash
go test ./internal/diff -run TestClassify -v
```