#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
NAMESPACE=${DOCKUBE_NAMESPACE:-dockube-workloads}
REGISTRY=${DOCKUBE_REGISTRY:-"$(minikube ip):30500"}
WORK=$(mktemp -d)
SERVER_PID=""

cleanup() {
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" >/dev/null 2>&1 || true
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT

mkdir -p "$WORK/tar-context" "$WORK/directory-context"
cat >"$WORK/tar-context/Dockerfile" <<'EOF'
FROM scratch
LABEL org.dockube.context=tar
EOF
tar -C "$WORK/tar-context" -cf "$WORK/context.tar" .

"$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  --push --registry-insecure \
  -t "$REGISTRY/dockube-build-context:tar" "$WORK/context.tar" >/dev/null

printf 'external-file\n' >"$WORK/directory-context/message.txt"
printf 'outside-secret\n' >"$WORK/outside-secret"
ln -s "$WORK/outside-secret" "$WORK/directory-context/external-link"
cat >"$WORK/External.Dockerfile" <<'EOF'
FROM scratch
COPY message.txt /message.txt
EOF
"$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  --push --registry-insecure -f "$WORK/External.Dockerfile" \
  -t "$REGISTRY/dockube-build-context:external-file" \
  "$WORK/directory-context" >/dev/null

tar -C "$WORK/tar-context" -cf - . | \
  "$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
    --push --registry-insecure \
    -t "$REGISTRY/dockube-build-context:stdin-tar" - >/dev/null

printf 'FROM scratch\nCOPY message.txt /message.txt\n' | \
  "$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
    --push --registry-insecure -f - \
    -t "$REGISTRY/dockube-build-context:stdin-dockerfile" \
    "$WORK/directory-context" >/dev/null

mkdir "$WORK/git-context"
printf 'FROM scratch\nLABEL org.dockube.context=git\n' >"$WORK/git-context/Dockerfile"
git -C "$WORK/git-context" init -q
git -C "$WORK/git-context" -c user.name=Test -c user.email=test@example.invalid \
  add Dockerfile
git -C "$WORK/git-context" -c user.name=Test -c user.email=test@example.invalid \
  commit -q -m initial
"$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  --push --registry-insecure \
  -t "$REGISTRY/dockube-build-context:git" \
  "file://$WORK/git-context" >/dev/null

cp "$WORK/context.tar" "$WORK/remote-context.tar"
printf 'FROM scratch\nLABEL org.dockube.context=remote-text\n' >"$WORK/RemoteTextDockerfile"
printf 'FROM scratch\nCOPY message.txt /message.txt\n' >"$WORK/RemoteFile.Dockerfile"
openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -subj /CN=localhost -addext 'subjectAltName=DNS:localhost,IP:127.0.0.1' \
  -keyout "$WORK/server.key" -out "$WORK/server.crt" >/dev/null 2>&1
python3 - "$WORK" "$WORK/server.port" <<'PY' &
import http.server
import os
import pathlib
import ssl
import sys

root, port_file = sys.argv[1:]
os.chdir(root)
server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), http.server.SimpleHTTPRequestHandler)
context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
context.load_cert_chain("server.crt", "server.key")
server.socket = context.wrap_socket(server.socket, server_side=True)
pathlib.Path(port_file).write_text(str(server.server_port))
server.serve_forever()
PY
SERVER_PID=$!
for _ in $(seq 1 50); do
  [ -s "$WORK/server.port" ] && break
  sleep 0.1
done
PORT=$(cat "$WORK/server.port")
for remote in remote-context.tar RemoteTextDockerfile; do
  SSL_CERT_FILE="$WORK/server.crt" "$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
    --push --registry-insecure \
    -t "$REGISTRY/dockube-build-context:remote-${remote,,}" \
    "https://127.0.0.1:$PORT/$remote" >/dev/null
done
SSL_CERT_FILE="$WORK/server.crt" "$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  --push --registry-insecure -f "https://127.0.0.1:$PORT/RemoteFile.Dockerfile" \
  -t "$REGISTRY/dockube-build-context:remote-file" \
  "$WORK/directory-context" >/dev/null

mkdir "$WORK/named-context" "$WORK/named-main"
printf 'named-context\n' >"$WORK/named-context/asset.txt"
cat >"$WORK/named-main/Dockerfile" <<'EOF'
FROM scratch
COPY --from=assets /asset.txt /asset.txt
EOF
"$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  --push --registry-insecure \
  --build-context "assets=$WORK/named-context" \
  -t "$REGISTRY/dockube-build-context:named" "$WORK/named-main" >/dev/null

if "$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  --push --registry-insecure -t ignored.invalid/http \
  "http://127.0.0.1:$PORT/remote-context.tar" >/dev/null 2>&1; then
  echo "unverified HTTP context was accepted" >&2
  exit 1
fi

mkfifo "$WORK/directory-context/special-pipe"
if "$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  --push --registry-insecure -f "$WORK/External.Dockerfile" \
  -t ignored.invalid/special "$WORK/directory-context" >/dev/null 2>&1; then
  echo "special context file was accepted" >&2
  exit 1
fi
rm "$WORK/directory-context/special-pipe"
printf 'unreadable\n' >"$WORK/directory-context/unreadable"
chmod 000 "$WORK/directory-context/unreadable"
if "$ROOT/bin/dockube" --namespace "$NAMESPACE" build \
  --push --registry-insecure -f "$WORK/External.Dockerfile" \
  -t ignored.invalid/unreadable "$WORK/directory-context" >/dev/null 2>&1; then
  echo "unreadable context file was accepted" >&2
  exit 1
fi

echo "local, stdin, named, Git, HTTPS, and external-Dockerfile context e2e passed"
