#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
REGISTRY=${DOCKUBE_REGISTRY:-"$(minikube ip):30500"}
CONTEXT=$(mktemp -d)
MANIFEST=$(mktemp)

cleanup() {
  rm -rf "$CONTEXT" "$MANIFEST"
}
trap cleanup EXIT

cat >"$CONTEXT/Dockerfile" <<'EOF'
FROM scratch
ARG TARGETOS
ARG TARGETARCH
LABEL org.dockube.test=multi-platform
EOF

IMAGE="$REGISTRY/dockube-build-multiplatform:test"
"$ROOT/bin/dockube" --namespace dockube-workloads build \
  --push --registry-insecure \
  --platform linux/amd64,linux/arm64 \
  -t "$IMAGE" "$CONTEXT" >/dev/null

curl --fail --silent --show-error \
  -H 'Accept: application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json' \
  "http://$REGISTRY/v2/dockube-build-multiplatform/manifests/test" >"$MANIFEST"

python3 - "$MANIFEST" <<'PY'
import json
import pathlib
import sys

index = json.loads(pathlib.Path(sys.argv[1]).read_text())
platforms = {
    (manifest["platform"]["os"], manifest["platform"]["architecture"])
    for manifest in index["manifests"]
}
assert platforms == {("linux", "amd64"), ("linux", "arm64")}, platforms
PY

echo "multi-platform build e2e passed: $IMAGE"
