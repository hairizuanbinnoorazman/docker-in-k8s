#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
NAMESPACE=${DOCKUBE_NAMESPACE:-dockube-workloads}
REGISTRY=${DOCKUBE_REGISTRY:-"$(minikube ip):30500"}
NAME="dockube-build-e2e-$$"
CONTEXT=$(mktemp -d)
BUILD_LOG=$(mktemp)

cleanup() {
  "$ROOT/bin/dockube" --namespace "$NAMESPACE" rm "$NAME" >/dev/null 2>&1 || true
  rm -rf "$CONTEXT" "$BUILD_LOG"
}
trap cleanup EXIT

cat >"$CONTEXT/Dockerfile" <<'EOF'
FROM busybox:1.36 AS base
ARG BUILD_MESSAGE=wrong
RUN test "$BUILD_MESSAGE" = configured && echo dockube-live-log-marker && sleep 3

FROM base AS release
CMD ["sh", "-c", "echo built-by-dockube"]

FROM busybox:1.36 AS should-not-build
RUN false
EOF

# This file is intentionally larger than the ConfigMap transport limit and is
# incompressible. The build can only reach the cluster if dockube applies
# .dockerignore before creating the context archive.
printf 'ignored.bin\n' >"$CONTEXT/.dockerignore"
head -c 800000 /dev/urandom >"$CONTEXT/ignored.bin"
# This second file is not ignored. Its compressed archive exceeds the former
# 700 KiB ConfigMap limit and verifies that the context is streamed instead.
head -c 800000 /dev/urandom >"$CONTEXT/included.bin"

IMAGE="$REGISTRY/dockube-build-e2e:dev"
ALT_IMAGE="$REGISTRY/dockube-build-e2e:latest"
BUILD_MESSAGE=configured "$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  --push \
  --registry-insecure \
  --build-arg BUILD_MESSAGE \
  --target release \
  --label org.dockube.test=build-e2e \
  --platform linux/amd64 \
  --no-cache \
  --no-cache-filter base \
  --pull \
  -t "$IMAGE" \
  -t "$ALT_IMAGE" \
  "$CONTEXT" >"$BUILD_LOG" 2>&1 &
BUILD_PID=$!
LIVE_LOG_SEEN=false
for _ in $(seq 1 100); do
  if grep -q dockube-live-log-marker "$BUILD_LOG"; then
    if ! kill -0 "$BUILD_PID" 2>/dev/null; then
      echo "BuildKit output only appeared after the build exited" >&2
      cat "$BUILD_LOG" >&2
      exit 1
    fi
    LIVE_LOG_SEEN=true
    break
  fi
  sleep 0.1
done
if [ "$LIVE_LOG_SEEN" != true ]; then
  echo "timed out waiting for live BuildKit output" >&2
  cat "$BUILD_LOG" >&2
  exit 1
fi
wait "$BUILD_PID"
cat "$BUILD_LOG"
"$ROOT/bin/dockube" --namespace "$NAMESPACE" run -d --name "$NAME" "$ALT_IMAGE" >/dev/null

for _ in $(seq 1 60); do
  if "$ROOT/bin/dockube" --namespace "$NAMESPACE" logs "$NAME" 2>/dev/null | grep -q built-by-dockube; then
    echo "build e2e passed: $IMAGE"
    exit 0
  fi
  sleep 1
done

echo "timed out waiting for the built image to run" >&2
exit 1
