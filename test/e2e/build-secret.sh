#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
REGISTRY=${DOCKUBE_REGISTRY:-"$(minikube ip):30500"}
CONTEXT=$(mktemp -d)
SECRET_FILE="$CONTEXT/local-secret.txt"
BUILD_LOG=$(mktemp)

cleanup() {
  rm -rf "$CONTEXT" "$SECRET_FILE" "$BUILD_LOG"
}
trap cleanup EXIT

FILE_VALUE=$(openssl rand -hex 24)
ENV_VALUE=$(openssl rand -hex 24)
printf '%s' "$FILE_VALUE" >"$SECRET_FILE"
FILE_HASH=$(printf '%s' "$FILE_VALUE" | sha256sum | cut -d' ' -f1)
ENV_HASH=$(printf '%s' "$ENV_VALUE" | sha256sum | cut -d' ' -f1)

cat >"$CONTEXT/Dockerfile" <<'EOF'
FROM busybox:1.36
ARG FILE_HASH
ARG ENV_HASH
COPY . /context
RUN test ! -e /context/local-secret.txt
RUN --mount=type=secret,id=file-secret \
    --mount=type=secret,id=env-secret \
    test "$(sha256sum /run/secrets/file-secret | cut -d' ' -f1)" = "$FILE_HASH" && \
    test "$(sha256sum /run/secrets/env-secret | cut -d' ' -f1)" = "$ENV_HASH"
EOF

IMAGE="$REGISTRY/dockube-build-secret-e2e:test"
BUILD_SECRET_ENV="$ENV_VALUE" "$ROOT/bin/dockube" --namespace dockube-workloads build \
  --push --registry-insecure \
  --build-arg "FILE_HASH=$FILE_HASH" \
  --build-arg "ENV_HASH=$ENV_HASH" \
  --secret "type=file,id=file-secret,src=$SECRET_FILE" \
  --secret type=env,id=env-secret,env=BUILD_SECRET_ENV \
  -t "$IMAGE" "$CONTEXT" >"$BUILD_LOG" 2>&1

if grep -Fq "$FILE_VALUE" "$BUILD_LOG" || grep -Fq "$ENV_VALUE" "$BUILD_LOG"; then
  echo "build secret value leaked into output" >&2
  cat "$BUILD_LOG" >&2
  exit 1
fi

echo "build secret e2e passed: $IMAGE"
