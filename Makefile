# Every target runs from a fresh clone with only Go (and for e2e: docker, kind, helm, kubectl, jq).
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
IMAGE   ?= ghcr.io/perserverance-syn/cluster-recorder
KIND_CLUSTER ?= recorder-e2e

.PHONY: build test test-race vet fmt-check docker helm-lint manifests manifests-check docs docs-check kind-up kind-down e2e

build:
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/collector ./cmd/collector
	CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o bin/agent ./cmd/agent
	go build -trimpath -ldflags "-s -w" -o bin/recorderctl ./cmd/recorderctl

test:
	go test ./...

# Race detector needs cgo (a C compiler); CI runs it on Linux.
test-race:
	go test -race ./...

vet:
	go vet ./...

fmt-check:
	@test -z "$$(gofmt -l .)" || { echo "gofmt needed on:"; gofmt -l .; exit 1; }

docker:
	docker build --build-arg VERSION=$(VERSION) -t $(IMAGE):$(VERSION) .

helm-lint:
	helm lint deploy/helm
	helm lint deploy/helm --set agent.enabled=true

# Plain-YAML install path, generated from the chart so the two never drift.
manifests:
	@printf 'apiVersion: v1\nkind: Namespace\nmetadata:\n  name: recorder\n  labels:\n    pod-security.kubernetes.io/enforce: restricted\n' > deploy/manifests/00-namespace.yaml
	helm template recorder deploy/helm --namespace recorder > deploy/manifests/10-collector.yaml

manifests-check: manifests
	@git diff --exit-code -- deploy/manifests || { echo "deploy/manifests is out of date: run make manifests"; exit 1; }

# docs/API.md is generated from the OpenAPI spec. Hand-edits are overwritten.
docs:
	go run ./hack/apidoc internal/api/openapi.yaml > docs/API.md

docs-check: docs
	@git diff --exit-code -- docs/API.md || { echo "docs/API.md is out of date: run make docs"; exit 1; }

kind-up:
	kind create cluster --name $(KIND_CLUSTER) --wait 120s

kind-down:
	kind delete cluster --name $(KIND_CLUSTER)

e2e:
	KIND_CLUSTER=$(KIND_CLUSTER) ./hack/e2e.sh