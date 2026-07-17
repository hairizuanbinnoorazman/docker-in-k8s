#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
NAMESPACE=${DOCKUBE_NAMESPACE:-dockube-workloads}
BUILD_NAMESPACE=${DOCKUBE_BUILD_NAMESPACE:-dockube-system}
REGISTRY=${DOCKUBE_REGISTRY:-"$(minikube ip):30500"}
CONTEXT=$(mktemp -d)
OUTPUT=$(mktemp)
LOG=$(mktemp)
KUBECTL=(minikube kubectl --)
BUILD_PID=""

cleanup() {
  if [ -n "$BUILD_PID" ]; then
    kill "$BUILD_PID" >/dev/null 2>&1 || true
  fi
  "${KUBECTL[@]}" -n "$BUILD_NAMESPACE" delete jobs \
    -l app.kubernetes.io/managed-by=dockube --ignore-not-found >/dev/null 2>&1 || true
  rm -rf "$CONTEXT" "$OUTPUT" "$LOG"
}
trap cleanup EXIT

assert_no_jobs() {
  local count
  count=$("${KUBECTL[@]}" -n "$BUILD_NAMESPACE" get jobs \
    -l app.kubernetes.io/managed-by=dockube --no-headers 2>/dev/null | wc -l)
  test "$count" -eq 0
}

printf 'FROM scratch\n' >"$CONTEXT/Dockerfile"
"$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  --push --registry-insecure \
  -t "$REGISTRY/dockube-build-cleanup:success" "$CONTEXT" >/dev/null
assert_no_jobs

cat >"$CONTEXT/Dockerfile" <<'EOF'
FROM busybox:1.36
RUN false
EOF
if "$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  -o "type=tar,dest=$OUTPUT.failed" "$CONTEXT" >"$LOG" 2>&1; then
  echo "expected failed build" >&2
  exit 1
fi
assert_no_jobs

cat >"$CONTEXT/Dockerfile" <<'EOF'
FROM busybox:1.36
RUN sleep 30
EOF
if "$ROOT/bin/dockube" --namespace "$NAMESPACE" build --timeout 2s \
  -o "type=tar,dest=$OUTPUT.timeout" "$CONTEXT" >"$LOG" 2>&1; then
  echo "expected timed-out build" >&2
  exit 1
fi
assert_no_jobs

"$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  -o "type=tar,dest=$OUTPUT.cancel" "$CONTEXT" >"$LOG" 2>&1 &
BUILD_PID=$!
for _ in $(seq 1 100); do
  if "${KUBECTL[@]}" -n "$BUILD_NAMESPACE" get jobs \
    -l app.kubernetes.io/managed-by=dockube --no-headers 2>/dev/null | grep -q .; then
    break
  fi
  sleep 0.1
done
kill -TERM "$BUILD_PID"
wait "$BUILD_PID" || true
BUILD_PID=""
for _ in $(seq 1 50); do
  assert_no_jobs && break
  sleep 0.1
done
assert_no_jobs

echo "build cleanup e2e passed"
