#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
REGISTRY=${DOCKUBE_REGISTRY:-"$(minikube ip):30500"}
CONTEXT=$(mktemp -d)
KEY_DIR=$(mktemp -d)
BUILD_LOG=$(mktemp)

cleanup() {
	status=$?
	if [ "$status" -ne 0 ] && [ -s "$BUILD_LOG" ]; then
		cat "$BUILD_LOG" >&2
	fi
  rm -rf "$CONTEXT" "$KEY_DIR" "$BUILD_LOG"
	exit "$status"
}
trap cleanup EXIT

ssh-keygen -q -t ed25519 -N '' -C dockube-build-ssh-e2e -f "$KEY_DIR/id_ed25519"

cat >"$CONTEXT/Dockerfile" <<'EOF'
FROM alpine:3.20
RUN apk add --no-cache openssh-client
RUN --mount=type=ssh,id=deploy ssh-add -L | grep -q '^ssh-ed25519 '
EOF

IMAGE="$REGISTRY/dockube-build-ssh-e2e:test"
"$ROOT/bin/dockube" --namespace dockube-workloads build \
  --push --registry-insecure \
  --ssh "deploy=$KEY_DIR/id_ed25519" \
  -t "$IMAGE" "$CONTEXT" >"$BUILD_LOG" 2>&1

KEY_FRAGMENT=$(sed -n '2p' "$KEY_DIR/id_ed25519")
if grep -Fq "$KEY_DIR/id_ed25519" "$BUILD_LOG" || grep -Fq "$KEY_FRAGMENT" "$BUILD_LOG"; then
  echo "SSH private key path or value leaked into output" >&2
  cat "$BUILD_LOG" >&2
  exit 1
fi

echo "build SSH e2e passed: $IMAGE"
