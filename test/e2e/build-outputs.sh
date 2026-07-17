#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
NAMESPACE=${DOCKUBE_NAMESPACE:-dockube-workloads}
REGISTRY=${DOCKUBE_REGISTRY:-"$(minikube ip):30500"}
WORK=$(mktemp -d)

cleanup() {
  rm -rf "$WORK"
}
trap cleanup EXIT

mkdir "$WORK/context"
printf 'dockube-output\n' >"$WORK/context/message.txt"
cat >"$WORK/context/Dockerfile" <<'EOF'
FROM scratch
COPY message.txt /message.txt
EOF

"$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  -o "type=oci,dest=$WORK/result.oci.tar" \
  -o "type=docker,dest=$WORK/result.docker.tar" \
  -o "type=tar,dest=$WORK/result.rootfs.tar" \
  -o "type=local,dest=$WORK/result-local" \
  "$WORK/context" >/dev/null

tar -tf "$WORK/result.oci.tar" | grep -qx 'index.json'
tar -tf "$WORK/result.docker.tar" | grep -qx 'manifest.json'
tar -tf "$WORK/result.rootfs.tar" | grep -Eq '^\.?/?message.txt$'
grep -qx dockube-output "$WORK/result-local/message.txt"

REGISTRY_IMAGE="$REGISTRY/dockube-build-output:registry"
"$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  --registry-insecure -o "type=registry,name=$REGISTRY_IMAGE" \
  "$WORK/context" >/dev/null
curl --fail --silent --show-error \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json' \
  "http://$REGISTRY/v2/dockube-build-output/manifests/registry" >/dev/null

echo "registry-independent repeatable outputs e2e passed"
