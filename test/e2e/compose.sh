#!/usr/bin/env bash
set -euo pipefail

namespace="${DOCKUBE_NAMESPACE:-dockube-workloads}"
binary="${DOCKUBE_BINARY:-./bin/dockube}"
fixture="${DOCKUBE_COMPOSE_FIXTURE:-test/e2e/compose/compose.yaml}"
updated="${DOCKUBE_COMPOSE_UPDATED_FIXTURE:-test/e2e/compose/compose.updated.yaml}"
invalid="${DOCKUBE_COMPOSE_INVALID_FIXTURE:-test/e2e/compose/unsupported.yaml}"
project="compose-e2e"
selector="dockube.io/compose-project=$project"

cleanup() {
  "$binary" --namespace "$namespace" compose -f "$fixture" down --volumes >/dev/null 2>&1 || true
  kubectl delete secret compose-e2e-token -n "$namespace" --ignore-not-found >/dev/null 2>&1 || true
}
wait_for_ready() {
  for _ in $(seq 1 90); do
    if test "$(kubectl get pod -n "$namespace" -l "$selector" -o name | wc -l)" -eq 2; then
      break
    fi
    sleep 1
  done
  test "$(kubectl get pod -n "$namespace" -l "$selector" -o name | wc -l)" -eq 2
  kubectl wait --for=condition=Ready pod -n "$namespace" -l "$selector" --timeout=180s
}

trap cleanup EXIT
cleanup

kubectl create secret generic compose-e2e-token -n "$namespace" --from-literal=api_token=secret-value >/dev/null
test "$(kubectl auth can-i get pods -n "$namespace" --as=system:serviceaccount:dockube-system:dockube-controller)" = yes
test "$(kubectl auth can-i get secrets -n "$namespace" --as=system:serviceaccount:dockube-system:dockube-controller)" = no

"$binary" compose -f "$fixture" config | grep -q 'name: compose-e2e'
if "$binary" --namespace "$namespace" compose -f "$invalid" up -d 2>invalid.err; then
  echo "unsupported privileged project unexpectedly succeeded" >&2
  exit 1
fi
grep -q 'unsupported Compose field' invalid.err
if kubectl get dockercontainers -n "$namespace" -l 'dockube.io/compose-project=compose-e2e-invalid' -o name | grep -q .; then
  echo "invalid project mutated the cluster" >&2
  exit 1
fi
rm -f invalid.err

"$binary" --namespace "$namespace" compose -f "$fixture" up -d
"$binary" --namespace "$namespace" compose -f "$fixture" up -d

test "$(kubectl get dockercontainers -n "$namespace" -l "$selector" -o name | wc -l)" -eq 2
test "$(kubectl get services -n "$namespace" -l "$selector" -o name | wc -l)" -eq 2
test "$(kubectl get pvc -n "$namespace" -l "$selector" -o name | wc -l)" -eq 1
wait_for_ready

for _ in $(seq 1 60); do
  if "$binary" --namespace "$namespace" compose -f "$fixture" logs client 2>/dev/null | grep -q 'version-one|config-value|secret-value|1'; then break; fi
  sleep 1
done
"$binary" --namespace "$namespace" compose -f "$fixture" logs client | grep -q 'version-one|config-value|secret-value|1'
"$binary" --namespace "$namespace" compose -f "$fixture" ps | grep -q 'web'
"$binary" --namespace "$namespace" compose -f "$fixture" ps | grep -q 'client'

web_pod="$(kubectl get pod -n "$namespace" -l "$selector,dockube.io/compose-service=web" -o jsonpath='{.items[0].metadata.name}')"
old_uid="$(kubectl get pod "$web_pod" -n "$namespace" -o jsonpath='{.metadata.uid}')"
test "$(kubectl get pod "$web_pod" -n "$namespace" -o jsonpath='{.spec.automountServiceAccountToken}')" = false
test "$(kubectl get pod "$web_pod" -n "$namespace" -o jsonpath='{.spec.securityContext.runAsNonRoot}')" = true
test "$(kubectl get pod "$web_pod" -n "$namespace" -o jsonpath='{.spec.containers[0].securityContext.allowPrivilegeEscalation}')" = false
test "$(kubectl get pod "$web_pod" -n "$namespace" -o jsonpath='{.spec.containers[0].securityContext.capabilities.drop[0]}')" = ALL
test "$(kubectl get pod "$web_pod" -n "$namespace" -o jsonpath='{.spec.automountServiceAccountToken}')" = false

"$binary" --namespace "$namespace" compose -f "$updated" up -d
for _ in $(seq 1 90); do
  new_uid="$(kubectl get pod -n "$namespace" -l "$selector,dockube.io/compose-service=web" -o jsonpath='{.items[0].metadata.uid}' 2>/dev/null || true)"
  if test -n "$new_uid" && test "$new_uid" != "$old_uid"; then break; fi
  sleep 1
done
test "$new_uid" != "$old_uid"
kubectl wait --for=condition=Ready pod -n "$namespace" -l "$selector,dockube.io/compose-service=web" --timeout=180s
kubectl exec -n "$namespace" "$(kubectl get pod -n "$namespace" -l "$selector,dockube.io/compose-service=web" -o jsonpath='{.items[0].metadata.name}')" -- cat /tmp/www/index.html | grep -q 'version-two|config-value|secret-value|2'

"$binary" --namespace "$namespace" compose -f "$updated" stop >/dev/null
for service in web client; do test "$(kubectl get dockercontainer "$project-$service" -n "$namespace" -o jsonpath='{.status.phase}')" = Stopped; done
"$binary" --namespace "$namespace" compose -f "$updated" start >/dev/null
wait_for_ready

restart_uid="$(kubectl get pod -n "$namespace" -l "$selector,dockube.io/compose-service=web" -o jsonpath='{.items[0].metadata.uid}')"
"$binary" --namespace "$namespace" compose -f "$updated" restart >/dev/null
wait_for_ready
new_restart_uid="$(kubectl get pod -n "$namespace" -l "$selector,dockube.io/compose-service=web" -o jsonpath='{.items[0].metadata.uid}')"
test "$restart_uid" != "$new_restart_uid"

"$binary" --namespace "$namespace" compose -f "$updated" down >/dev/null
if kubectl get dockercontainers -n "$namespace" -l "$selector" -o name | grep -q .; then echo "workloads remain after down" >&2; exit 1; fi
if kubectl get services -n "$namespace" -l "$selector" -o name | grep -q .; then echo "Services remain after down" >&2; exit 1; fi
test "$(kubectl get pvc -n "$namespace" -l "$selector" -o name | wc -l)" -eq 1

"$binary" --namespace "$namespace" compose -f "$updated" up -d >/dev/null
wait_for_ready
kubectl exec -n "$namespace" "$(kubectl get pod -n "$namespace" -l "$selector,dockube.io/compose-service=web" -o jsonpath='{.items[0].metadata.name}')" -- cat /tmp/www/index.html | grep -Eq 'version-two\|config-value\|secret-value\|[3-9][0-9]*'
"$binary" --namespace "$namespace" compose -f "$updated" down --volumes >/dev/null
if kubectl get pvc -n "$namespace" -l "$selector" -o name | grep -q .; then echo "PVC remains after down --volumes" >&2; exit 1; fi

kubectl delete secret compose-e2e-token -n "$namespace" --ignore-not-found >/dev/null
trap - EXIT
