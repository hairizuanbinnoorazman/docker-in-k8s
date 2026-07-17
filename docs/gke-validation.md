# GKE validation

`dockube` was validated on GKE Standard on June 21, 2026.

## Environment

| Component | Version or configuration |
| --- | --- |
| Kubernetes server | `v1.35.5-gke.1000000` |
| `kubectl` client | `v1.35.5` |
| GKE mode | Standard |
| Persistent storage | Default GKE `standard-rwo` CSI StorageClass |
| Controller image | Built from `Dockerfile.controller` and pulled from a private Artifact Registry repository |

Project, account, cluster, endpoint, token, registry path, Pod identifiers, and
Secret values are intentionally omitted from this report.

## GKE deployment requirement

The checked-in `deploy/install.yaml` is configured for Minikube development:

```yaml
image: dockube-controller:dev
imagePullPolicy: Never
```

GKE nodes cannot use an image stored only in the developer workstation's local
Docker daemon. Build and push the controller to a registry that the GKE nodes
can read, then replace the image and pull policy during deployment:

```bash
export CONTROLLER_IMAGE=REGISTRY/PROJECT/REPOSITORY/dockube-controller:TAG

kubectl apply -f deploy/crd.yaml
sed \
  -e "s#image: dockube-controller:dev#image: ${CONTROLLER_IMAGE}#" \
  -e 's/imagePullPolicy: Never/imagePullPolicy: IfNotPresent/' \
  deploy/install.yaml |
  kubectl apply -f -

kubectl rollout status deployment/dockube-controller \
  --namespace dockube-system \
  --timeout=180s
```

Use an immutable tag or digest for repeatable deployments.

## Validation procedure

The repository's standard checks and unchanged end-to-end scripts were run:

```bash
go test ./...
go vet ./...
make build
bash test/e2e/lifecycle.sh
bash test/e2e/compose.sh
```

The controller was deployed using the GKE-accessible image override described
above. The tests used the default `dockube-system` and `dockube-workloads`
namespaces because the Compose test verifies the controller ServiceAccount by
its default namespace and name.

## Coverage and result

| Area | Checks performed | Result |
| --- | --- | --- |
| Controller | CRD installation, image pull, Deployment rollout, reconciliation | Pass |
| RBAC | Controller can read/create managed Pods but cannot read Secrets | Pass |
| Lifecycle | `run`, `ps`, `logs`, `exec`, `stop`, `start`, `restart`, `rm` | Pass |
| Security | Non-root UID, `RuntimeDefault` seccomp, dropped capabilities, no privilege escalation, no workload API token | Pass |
| Compose validation | Unsupported privileged input rejected before cluster mutation | Pass |
| Compose resources | DockerContainer CRs, ClusterIP Services, ConfigMaps, external Secret references, PVCs | Pass |
| Networking | Service-name DNS and in-cluster HTTP connectivity | Pass |
| Storage | GKE CSI PVC creation, data persistence across replacement Pods, volume deletion | Pass |
| Updates | Repeated `compose up -d`, config update, replacement Pod creation | Pass |
| Compose lifecycle | `ps`, `logs`, `stop`, `start`, `restart`, `down`, `down --volumes` | Pass |
| Cleanup | No managed workload resources remained after the suites | Pass |

## Issues found and fixed

The first GKE run exposed two portability defects:

1. An immediate second `compose up -d` could race with a controller update and
   fail with Kubernetes `409 Conflict`. Compose workload updates now use
   `RetryOnConflict`.
2. GKE Persistent Disk volumes were mounted as root-owned filesystems while
   workloads run as UID `65532`. Generated Pods now set `fsGroup: 65532`, which
   lets the non-root workload write to mounted PVCs.

Regression tests cover both changes.

## Sanitized evidence

The following output was captured from the final successful run. Identifiers
that are not relevant to compatibility were replaced or omitted. The complete
sanitized transcript is stored in
[`gke-validation-2026-06-21.log`](gke-validation-2026-06-21.log).

```text
GKE validation evidence (sanitized)
Date: 2026-06-21
Kubernetes server: v1.35.5-gke.1000000
kubectl client: v1.35.5

[1/6] Local unit tests
RESULT: PASS - go test ./...

[2/6] Static analysis
RESULT: PASS - go vet ./...

[3/6] Install CRD and GKE controller deployment
deployment "dockube-controller" successfully rolled out
NAME                 READY   AVAILABLE
dockube-controller   1       1
RESULT: PASS - controller available on GKE

[4/6] Controller RBAC boundary
get pods: yes
get secrets: no
RESULT: PASS - required Pod access granted; Secret access denied

[5/6] Docker-like lifecycle end-to-end test
pod/e2e-lifecycle-<id> condition met
pod/e2e-lifecycle-<id> condition met
pod/e2e-lifecycle-<id> condition met
RESULT: PASS - lifecycle.sh

[6/6] Compose end-to-end test
client  created or updated
web     created or updated
client  created or updated
web     created or updated
pod/compose-e2e-client-<id> condition met
pod/compose-e2e-web-<id> condition met
RESULT: PASS - compose.sh

Final managed-resource count:
0
OVERALL RESULT: PASS
```

The validation namespaces and CRD were deleted after the run.
