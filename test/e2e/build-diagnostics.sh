#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
CONTEXT=$(mktemp -d)
DENIED_NAMESPACE=dockube-build-denied
KUBECTL=(minikube kubectl --)

cleanup() {
  "${KUBECTL[@]}" delete namespace "$DENIED_NAMESPACE" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  rm -rf "$CONTEXT"
}
trap cleanup EXIT

cat >"$CONTEXT/Dockerfile" <<'EOF'
FROM scratch
EOF

if output=$("$ROOT/bin/dockube" --namespace dockube-workloads build \
  --push \
  --registry-insecure \
  --registry-secret dockube-build-secret-that-does-not-exist \
  --timeout 30s \
  -t registry.invalid/dockube/diagnostics:test "$CONTEXT" 2>&1); then
  echo "build unexpectedly started with a missing registry Secret" >&2
  exit 1
fi
if ! grep -Eq 'BuildKit Pod startup failed:.*(FailedMount|secret.*not found)' <<<"$output"; then
  echo "missing Secret diagnostic was not reported" >&2
  echo "$output" >&2
  exit 1
fi

"${KUBECTL[@]}" create namespace "$DENIED_NAMESPACE" >/dev/null
"${KUBECTL[@]}" create quota deny-build-pods -n "$DENIED_NAMESPACE" --hard=pods=0 >/dev/null

if output=$("$ROOT/bin/dockube" --namespace dockube-workloads build \
  --push \
  --build-namespace "$DENIED_NAMESPACE" \
  --registry-insecure \
  --timeout 30s \
  -t registry.invalid/dockube/diagnostics:test "$CONTEXT" 2>&1); then
  echo "build unexpectedly created a Pod despite the zero-Pod quota" >&2
  exit 1
fi
if ! grep -Eq 'BuildKit Job could not create a Pod:.*(FailedCreate|exceeded quota)' <<<"$output"; then
  echo "Job admission/quota diagnostic was not reported" >&2
  echo "$output" >&2
  exit 1
fi

echo "build diagnostics e2e passed"
