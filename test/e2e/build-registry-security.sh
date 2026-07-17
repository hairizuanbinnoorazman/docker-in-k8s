#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
NAMESPACE=dockube-system
REGISTRY=dockube-build-registry-tls.dockube-system.svc:5443
CONTEXT=$(mktemp -d)
CERTS=$(mktemp -d)
KUBECTL=(minikube kubectl --)

cleanup() {
  "${KUBECTL[@]}" delete -f "$ROOT/test/e2e/build-registry.yaml" --ignore-not-found >/dev/null 2>&1 || true
  "${KUBECTL[@]}" delete secret -n "$NAMESPACE" \
    dockube-build-registry-server-tls \
    dockube-build-registry-server-auth \
    dockube-build-registry-client-auth \
    dockube-build-registry-client-ca \
    --ignore-not-found >/dev/null 2>&1 || true
  rm -rf "$CONTEXT" "$CERTS"
}
trap cleanup EXIT

cat >"$CERTS/cert.conf" <<'EOF'
[req]
distinguished_name = subject
x509_extensions = extensions
prompt = no

[subject]
CN = dockube-build-registry-tls.dockube-system.svc

[extensions]
subjectAltName = @names
basicConstraints = critical,CA:TRUE
keyUsage = critical,digitalSignature,keyEncipherment,keyCertSign
extendedKeyUsage = serverAuth

[names]
DNS.1 = dockube-build-registry-tls
DNS.2 = dockube-build-registry-tls.dockube-system
DNS.3 = dockube-build-registry-tls.dockube-system.svc
DNS.4 = dockube-build-registry-tls.dockube-system.svc.cluster.local
EOF

openssl req -x509 -newkey rsa:2048 -nodes -days 1 \
  -config "$CERTS/cert.conf" \
  -keyout "$CERTS/tls.key" \
  -out "$CERTS/tls.crt" >/dev/null 2>&1
# Registry 2.x requires bcrypt entries. This is a fixed test-only hash for the
# password "dockube-password".
printf '%s\n' 'dockube:$2y$05$6KOD/GWHpzPl8WmyAgH4zu4/.2SRRMrudDBdaU8XA9Hxsbgk/c4JO' >"$CERTS/htpasswd"

"${KUBECTL[@]}" create secret tls dockube-build-registry-server-tls \
  -n "$NAMESPACE" --cert="$CERTS/tls.crt" --key="$CERTS/tls.key" >/dev/null
"${KUBECTL[@]}" create secret generic dockube-build-registry-server-auth \
  -n "$NAMESPACE" --from-file=htpasswd="$CERTS/htpasswd" >/dev/null
"${KUBECTL[@]}" create secret docker-registry dockube-build-registry-client-auth \
  -n "$NAMESPACE" --docker-server="$REGISTRY" --docker-username=dockube \
  --docker-password=dockube-password >/dev/null
"${KUBECTL[@]}" create secret generic dockube-build-registry-client-ca \
  -n "$NAMESPACE" --from-file=ca.crt="$CERTS/tls.crt" >/dev/null
"${KUBECTL[@]}" apply -f "$ROOT/test/e2e/build-registry.yaml" >/dev/null
"${KUBECTL[@]}" rollout status deployment/dockube-build-registry-tls \
  -n "$NAMESPACE" --timeout=120s >/dev/null

cat >"$CONTEXT/Dockerfile" <<'EOF'
FROM scratch
LABEL org.dockube.test=registry-security
EOF

IMAGE="$REGISTRY/dockube/security:test"

if "$ROOT/bin/dockube" --namespace dockube-workloads build \
  --push \
  --registry-secret dockube-build-registry-client-auth \
  -t "$IMAGE" "$CONTEXT" >/dev/null 2>&1; then
  echo "build unexpectedly trusted the private registry CA" >&2
  exit 1
fi

if "$ROOT/bin/dockube" --namespace dockube-workloads build \
  --push \
  --registry-ca-secret dockube-build-registry-client-ca \
  -t "$IMAGE" "$CONTEXT" >/dev/null 2>&1; then
  echo "build unexpectedly pushed without registry credentials" >&2
  exit 1
fi

"$ROOT/bin/dockube" --namespace dockube-workloads build \
  --push \
  --registry-ca-secret dockube-build-registry-client-ca \
  --registry-secret dockube-build-registry-client-auth \
  -t "$IMAGE" "$CONTEXT"

echo "secure registry build e2e passed: $IMAGE"
