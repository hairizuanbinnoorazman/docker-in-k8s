#!/usr/bin/env bash
set -euo pipefail

namespace="${DOCKUBE_NAMESPACE:-dockube-workloads}"
binary="${DOCKUBE_BINARY:-./bin/dockube}"
name="e2e-lifecycle"

cleanup() {
  "$binary" -n "$namespace" rm "$name" >/dev/null 2>&1 || true
}
trap cleanup EXIT

cleanup

id="$("$binary" -n "$namespace" run -d --name "$name" busybox:1.36 sleep 300)"
test "${#id}" -eq 12

for _ in $(seq 1 60); do
  if kubectl get pod -l "dockube.io/container-name=$name" -n "$namespace" -o name | grep -q .; then
    break
  fi
  sleep 1
done

if ! kubectl get pod -l "dockube.io/container-name=$name" -n "$namespace" -o name | grep -q .; then
  echo "controller did not create a Pod" >&2
  exit 1
fi

kubectl wait --for=condition=Ready pod \
  -l "dockube.io/container-name=$name" \
  -n "$namespace" \
  --timeout=120s

for _ in $(seq 1 30); do
  if test "$(kubectl get dockercontainer "$name" -n "$namespace" -o jsonpath='{.status.phase}')" = "Running"; then
    break
  fi
  sleep 1
done

test "$(kubectl get dockercontainer "$name" -n "$namespace" -o jsonpath='{.status.phase}')" = "Running"

"$binary" -n "$namespace" ps | grep -q "$name"
"$binary" -n "$namespace" ps -a | grep -q "$name"

test "$(kubectl get pod -l "dockube.io/container-name=$name" -n "$namespace" -o jsonpath='{.items[0].spec.automountServiceAccountToken}')" = "false"
test "$(kubectl get pod -l "dockube.io/container-name=$name" -n "$namespace" -o jsonpath='{.items[0].spec.securityContext.runAsNonRoot}')" = "true"
test "$(kubectl get pod -l "dockube.io/container-name=$name" -n "$namespace" -o jsonpath='{.items[0].spec.containers[0].securityContext.allowPrivilegeEscalation}')" = "false"

"$binary" -n "$namespace" rm "${id:0:6}" | grep -q "$name"

if kubectl get pod -l "dockube.io/container-name=$name" -n "$namespace" -o name | grep -q .; then
  echo "dependent Pod remains after rm" >&2
  exit 1
fi

trap - EXIT
