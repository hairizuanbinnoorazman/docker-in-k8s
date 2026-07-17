#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
NAMESPACE=${DOCKUBE_NAMESPACE:-dockube-workloads}
REGISTRY=${DOCKUBE_REGISTRY:-"$(minikube ip):30500"}
CONTEXT=$(mktemp -d)
MANIFEST=$(mktemp)
DEBUG_LOG=$(mktemp)

cleanup() {
  rm -rf "$CONTEXT" "$MANIFEST" "$DEBUG_LOG"
}
trap cleanup EXIT

cat >"$CONTEXT/Dockerfile" <<'EOF'
# syntax=docker/dockerfile:1
FROM busybox:1.36 AS release
ARG VERSION=1
LABEL org.dockube.version=$VERSION
RUN echo advanced > /advanced.txt
EOF

for call in check outline targets; do
  "$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
    --call "$call" "$CONTEXT" >/dev/null
done
"$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  --check "$CONTEXT" >/dev/null

DEBUG_SECRET="dockube-debug-secret-$RANDOM-$RANDOM"
"$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  --debug --check --build-arg "DEBUG_SECRET=$DEBUG_SECRET" \
  "$CONTEXT" >"$DEBUG_LOG" 2>&1
grep -q 'dockube build debug: job=' "$DEBUG_LOG"
if grep -q "$DEBUG_SECRET" "$DEBUG_LOG"; then
  echo "debug output leaked a build argument value" >&2
  exit 1
fi

IMAGE="$REGISTRY/dockube-build-advanced:test"
"$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  --push --registry-insecure -t "$IMAGE" \
  --annotation 'index:org.dockube.annotation=verified' \
  --provenance=mode=max --sbom=true \
  "$CONTEXT" >/dev/null

curl --fail --silent --show-error \
  -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json' \
  "http://$REGISTRY/v2/dockube-build-advanced/manifests/test" >"$MANIFEST"
python3 - "$MANIFEST" <<'PY'
import json
import pathlib
import sys

index = json.loads(pathlib.Path(sys.argv[1]).read_text())
assert index["annotations"]["org.dockube.annotation"] == "verified", index
attestations = [
    item for item in index["manifests"]
    if item.get("annotations", {}).get("vnd.docker.reference.type") == "attestation-manifest"
]
assert attestations, index
PY

echo "annotations, attestations, and Dockerfile calls e2e passed: $IMAGE"
