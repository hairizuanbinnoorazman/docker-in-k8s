#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
NAMESPACE=${DOCKUBE_NAMESPACE:-dockube-workloads}
REGISTRY=${DOCKUBE_REGISTRY:-"$(minikube ip):30500"}
CONTEXT=$(mktemp -d)
ERROR_LOG=$(mktemp)

cleanup() {
  rm -rf "$CONTEXT" "$ERROR_LOG"
}
trap cleanup EXIT

cat >"$CONTEXT/Dockerfile" <<'EOF'
FROM busybox:1.36
RUN grep -Eq '203[.]0[.]113[.]10[[:space:]]+example[.]internal' /etc/hosts
RUN test "$(df -k /dev/shm | awk 'NR == 2 { print $2 }')" -eq 32768
RUN ! wget -q -T 2 -O /dev/null http://example.com
EOF

IMAGE="$REGISTRY/dockube-build-network:test"
"$ROOT/bin/dockube" --namespace "$NAMESPACE" build --push --registry-insecure \
  --network none --add-host example.internal=203.0.113.10 --shm-size 32m \
  -t "$IMAGE" "$CONTEXT" >/dev/null

assert_rejected() {
  local expected=$1
  shift
  if "$ROOT/bin/dockube" --namespace "$NAMESPACE" build "$@" -t ignored.invalid/test "$CONTEXT" >"$ERROR_LOG" 2>&1; then
    echo "expected build option to be rejected: $*" >&2
    exit 1
  fi
  if ! grep -q -- "$expected" "$ERROR_LOG"; then
    cat "$ERROR_LOG" >&2
    exit 1
  fi
}

assert_rejected 'Kubernetes node network' --network host
assert_rejected 'does not grant host networking' --allow network.host
assert_rejected 'host cgroups' --cgroup-parent /docker
assert_rejected 'no Docker Engine image store' --load
assert_rejected 'only its default Kubernetes builder' --builder remote
assert_rejected 'administrator-controlled policy' --policy policy.json
assert_rejected 'Windows or legacy-builder options' --isolation hyperv
assert_rejected 'host-gateway would expose' --add-host host.docker.internal=host-gateway
assert_rejected 'administrator-defined Kubernetes requests and limits' --resource cpu=4
assert_rejected 'does not change node or container-runtime process limits' --ulimit nofile=1024:1024

echo "network and security controls e2e passed: $IMAGE"
