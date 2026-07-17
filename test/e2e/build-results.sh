#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
REGISTRY=${DOCKUBE_REGISTRY:-"$(minikube ip):30500"}
CONTEXT=$(mktemp -d)
METADATA=$(mktemp)
IID=$(mktemp)
OUTPUT=$(mktemp)

cleanup() {
  rm -rf "$CONTEXT" "$METADATA" "$IID" "$OUTPUT"
}
trap cleanup EXIT

cat >"$CONTEXT/Dockerfile" <<'EOF'
FROM scratch
LABEL org.dockube.test=build-results
EOF

"$ROOT/bin/dockube" --namespace dockube-workloads build \
  --push --registry-insecure --progress=plain \
  --metadata-file "$METADATA" --iidfile "$IID" \
  -t "$REGISTRY/dockube-build-results:metadata" "$CONTEXT" >"$OUTPUT"

python3 - "$METADATA" "$IID" <<'PY'
import json
import pathlib
import sys

metadata = json.loads(pathlib.Path(sys.argv[1]).read_text())
iid = pathlib.Path(sys.argv[2]).read_text().strip()
assert iid.startswith("sha256:")
assert iid == metadata["containerimage.config.digest"]
assert metadata["containerimage.digest"].startswith("sha256:")
PY
grep -q '^#' "$OUTPUT"

"$ROOT/bin/dockube" --namespace dockube-workloads build \
  --push --registry-insecure -q \
  -t "$REGISTRY/dockube-build-results:quiet" "$CONTEXT" >"$OUTPUT"
if ! grep -Eq '^sha256:[0-9a-f]{64}$' "$OUTPUT" || [ "$(wc -l <"$OUTPUT")" -ne 1 ]; then
  echo "quiet output was not exactly one image ID" >&2
  cat "$OUTPUT" >&2
  exit 1
fi

"$ROOT/bin/dockube" --namespace dockube-workloads build \
  --push --registry-insecure --progress=rawjson \
  -t "$REGISTRY/dockube-build-results:rawjson" "$CONTEXT" >"$OUTPUT"
python3 - "$OUTPUT" <<'PY'
import json
import pathlib
import sys

lines = [line for line in pathlib.Path(sys.argv[1]).read_text().splitlines() if line]
assert lines
for line in lines:
    json.loads(line)
PY

echo "build result modes e2e passed"
