#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
REGISTRY=${DOCKUBE_REGISTRY:-"$(minikube ip):30500"}
CONTEXT=$(mktemp -d)
FIRST_LOG=$(mktemp)
SECOND_LOG=$(mktemp)
KUBECTL=(minikube kubectl --)

cleanup() {
  "${KUBECTL[@]}" delete -f "$ROOT/test/e2e/build-cache-pvc.yaml" --ignore-not-found >/dev/null 2>&1 || true
  rm -rf "$CONTEXT" "$FIRST_LOG" "$SECOND_LOG"
}
trap cleanup EXIT

cat >"$CONTEXT/Dockerfile" <<'EOF'
FROM busybox:1.36
RUN echo dockube-cache-marker > /dockube-cache-marker && sleep 1
EOF

"${KUBECTL[@]}" apply -f "$ROOT/test/e2e/build-cache-pvc.yaml" >/dev/null
"${KUBECTL[@]}" wait -n dockube-system \
  --for=jsonpath='{.status.phase}'=Bound pvc/dockube-build-cache-e2e \
  --timeout=120s >/dev/null

IMAGE="$REGISTRY/dockube-build-cache-e2e:pvc"
for log in "$FIRST_LOG" "$SECOND_LOG"; do
  "$ROOT/bin/dockube" --namespace dockube-workloads build \
    --push --registry-insecure \
    --cache-pvc dockube-build-cache-e2e \
    -t "$IMAGE" "$CONTEXT" >"$log" 2>&1
done
if ! grep -q 'CACHED' "$SECOND_LOG"; then
  echo "second PVC-backed build did not use persistent cache" >&2
  cat "$SECOND_LOG" >&2
  exit 1
fi

CACHE_IMAGE="$REGISTRY/dockube-build-cache-e2e:registry-cache"
EXPORT_IMAGE="$REGISTRY/dockube-build-cache-e2e:export"
IMPORT_IMAGE="$REGISTRY/dockube-build-cache-e2e:import"
"$ROOT/bin/dockube" --namespace dockube-workloads build \
  --push --registry-insecure \
  --cache-to "$CACHE_IMAGE" \
  -t "$EXPORT_IMAGE" "$CONTEXT" >"$FIRST_LOG" 2>&1
"$ROOT/bin/dockube" --namespace dockube-workloads build \
  --push --registry-insecure \
  --cache-from "$CACHE_IMAGE" \
  -t "$IMPORT_IMAGE" "$CONTEXT" >"$SECOND_LOG" 2>&1
if ! grep -q 'CACHED' "$SECOND_LOG"; then
  echo "registry cache import did not produce a cache hit" >&2
  cat "$SECOND_LOG" >&2
  exit 1
fi

if "$ROOT/bin/dockube" --namespace dockube-workloads build \
  --push --cache-from type=local,src=/cache \
  -t ignored.invalid/cache "$CONTEXT" >"$SECOND_LOG" 2>&1; then
  echo "unsupported local cache backend was accepted" >&2
  exit 1
fi
grep -q 'credential and retention policies are undefined' "$SECOND_LOG"

echo "build cache e2e passed"
