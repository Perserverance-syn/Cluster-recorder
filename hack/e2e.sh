#!/usr/bin/env bash
# End-to-end against a kind cluster: build, load, install with defaults,
# then assert the four things INSTALL.md promises.
set -euo pipefail
CLUSTER="${KIND_CLUSTER:-recorder-e2e}"
NS=recorder
PORT="${E2E_PORT:-18080}"

docker build -t cluster-recorder:e2e .
kind load docker-image cluster-recorder:e2e --name "$CLUSTER"
helm upgrade --install recorder deploy/helm --namespace "$NS" --create-namespace \
  --set image.repository=cluster-recorder --set image.tag=e2e --wait --timeout 180s

kubectl -n "$NS" port-forward svc/recorder "$PORT:8080" >/dev/null 2>&1 &
PF=$!
trap 'kill $PF 2>/dev/null || true; kubectl delete pod e2e-fail --ignore-not-found >/dev/null 2>&1 || true' EXIT
sleep 3

echo "1. healthz"
curl -fsS "localhost:$PORT/api/v1/healthz" | grep -q ok

echo "2. events accumulate past the API (trigger some)"
kubectl run e2e-fail --image=busybox:1.36 --restart=Never -- sh -c 'exit 1' >/dev/null
n=0
for _ in $(seq 1 30); do
  n=$(curl -fsS "localhost:$PORT/api/v1/events?from=-10m" | jq length)
  [ "$n" -gt 0 ] && break
  sleep 2
done
[ "$n" -gt 0 ] || { echo "no events recorded"; exit 1; }

NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
echo "3. timeline for $NODE"
curl -fsS "localhost:$PORT/api/v1/nodes/$NODE/timeline?from=-1h" | jq -e 'type=="array"' >/dev/null

echo "4. diff degrades cleanly without agent data"
curl -fsS "localhost:$PORT/api/v1/nodes/$NODE/diff" | jq -e '.sources.api and (.sources.agent | not)' >/dev/null

echo "5. agent: enable, expect agent-sourced fields on every node"
helm upgrade recorder deploy/helm --namespace "$NS" --reset-then-reuse-values --set agent.enabled=true --wait --timeout 120s
kubectl -n recorder-agent rollout status ds/recorder-agent --timeout=120s
want=$(kubectl get nodes --no-headers | wc -l)
for _ in $(seq 1 30); do
  have=$(curl -fsS "localhost:$PORT/api/v1/nodes" | jq '[.[] | select(.last_agent_snapshot != null)] | length')
  [ "$have" = "$want" ] && break
  sleep 2
done
[ "$have" = "$want" ] || { echo "agent snapshots: $have of $want nodes"; exit 1; }
curl -fsS "localhost:$PORT/api/v1/nodes/$NODE/diff" | jq -e '[.sources.agent.fields[].field] | index("pod_bridge")' >/dev/null
echo "   driver on $NODE: $(curl -fsS "localhost:$PORT/api/v1/nodes" | jq -r ".[] | select(.node==\"$NODE\") | .driver")"
echo "e2e OK"