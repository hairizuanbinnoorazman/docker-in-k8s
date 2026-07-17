#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
NAMESPACE=${DOCKUBE_NAMESPACE:-dockube-workloads}
REGISTRY=${DOCKUBE_REGISTRY:-"$(minikube ip):30500"}
NAME="dockube-build-e2e-$$"
CONTEXT=$(mktemp -d)

cleanup() {
  "$ROOT/bin/dockube" --namespace "$NAMESPACE" rm "$NAME" >/dev/null 2>&1 || true
  rm -rf "$CONTEXT"
}
trap cleanup EXIT

cat >"$CONTEXT/Dockerfile" <<'EOF'
FROM busybox:1.36
CMD ["sh", "-c", "echo built-by-dockube"]
EOF

IMAGE="$REGISTRY/dockube-build-e2e:dev"
"$ROOT/bin/dockube" --namespace "$NAMESPACE" build -t "$IMAGE" "$CONTEXT"
"$ROOT/bin/dockube" --namespace "$NAMESPACE" run -d --name "$NAME" "$IMAGE" >/dev/null

for _ in $(seq 1 60); do
  if "$ROOT/bin/dockube" --namespace "$NAMESPACE" logs "$NAME" 2>/dev/null | grep -q built-by-dockube; then
    echo "build e2e passed: $IMAGE"
    exit 0
  fi
  sleep 1
done

echo "timed out waiting for the built image to run" >&2
exit 1
