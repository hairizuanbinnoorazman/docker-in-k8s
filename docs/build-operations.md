# Build namespace operations

`dockube build` creates a short-lived rootless BuildKit Job in
`dockube-system` by default. The CLI uses the current kubeconfig identity; the
dockube controller ServiceAccount does not create or manage these Jobs.

## Required RBAC

Grant build users the following namespaced permissions in the selected
`--build-namespace`:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: dockube-builder
rules:
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["create", "get", "delete"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
  - apiGroups: [""]
    resources: ["pods/exec"]
    verbs: ["create"]
  - apiGroups: [""]
    resources: ["events"]
    verbs: ["list"]
```

Bind this Role only to identities allowed to build. Registry and CA Secrets
and cache PVCs are referenced by name in the Job. Kubernetes admission and the
kubelet must be able to mount them; do not grant CLI users broad Secret read
access merely for dockube.

## Pod Security and admission

The pinned rootless BuildKit worker runs as UID/GID 1000 and does not request
host networking, host PID/IPC, privileged mode, capabilities, or devices. Its
rootless no-process-sandbox mode does require `allowPrivilegeEscalation: true`
and unconfined seccomp and AppArmor profiles. Consequently, a namespace that
enforces the Kubernetes Restricted Pod Security Standard rejects build Jobs.

Use a dedicated build namespace with a narrowly reviewed Pod Security
exemption or an admission policy scoped to Jobs labeled
`app.kubernetes.io/managed-by=dockube`. Keep workload namespaces Restricted.
Do not grant generic privileged, host-namespace, or host-device exemptions.

Other admission systems must permit the pinned image, the generated resource
limits, `emptyDir` volumes, approved Secret/PVC references, and the rootless
security context. Dockube reports warning Events when admission or scheduling
fails.

## Quotas and storage

Each Job requests 250m CPU, 256 MiB memory, and 1 GiB ephemeral storage, with
limits of 2 CPU, 2 GiB memory, and 10 GiB ephemeral storage. Account for these
values in ResourceQuota and LimitRange policies. Contexts, result handoff
files, and non-persistent BuildKit state use `emptyDir`; build secret and SSH
material uses a memory-backed `emptyDir`.

`--cache-pvc` mounts an existing PVC as persistent BuildKit state. Size,
StorageClass, access mode, backup, tenant isolation, and deletion are
administrator responsibilities. Do not share a cache PVC between mutually
untrusted builders. Registry caches persist according to registry retention
policy. Unsupported local/cloud cache backends are rejected.

## Network policy

Build Pods need no inbound application traffic. Permit only required egress:

- cluster DNS;
- source-image and output registries;
- package repositories or other endpoints used by Dockerfile `RUN` steps;
- scanner images/endpoints required by an enabled SBOM attestation.

`--network=none` disables network access for Dockerfile `RUN` steps, but image
resolution and registry export still require BuildKit daemon egress. Network
policy remains the administrator-enforced boundary. Host networking and
`host-gateway` mappings are rejected.

## Registry credentials and trust

TLS verification is on by default. Create a Docker config Secret and reference
it without copying credentials into CLI arguments:

```console
kubectl -n dockube-system create secret docker-registry build-registry-auth \
  --docker-server=registry.example.com \
  --docker-username=USER \
  --docker-password=PASSWORD

dockube build --registry-secret build-registry-auth --push \
  -t registry.example.com/team/image:tag .
```

For a private CA, create a Secret whose key is exactly `ca.crt` and pass its
name with `--registry-ca-secret`. Rotate Secrets through Kubernetes and registry
procedures. `--registry-insecure` enables unverified HTTP explicitly and should
be blocked by policy outside controlled development registries.

## Retention and cleanup

Jobs and their Pods are deleted after success, failure, timeout, SIGINT, or
SIGTERM. `--keep-build` intentionally retains the Job, Pod, logs, context
`emptyDir`, and result files until an administrator deletes the Job. PVC and
registry cache data always outlive a Job. Client OCI/Docker/tar/local outputs
remain at their requested destination. Configure Kubernetes TTL or an external
janitor as defense in depth for clients that are force-killed before graceful
cleanup.

Build arguments are not secrets. Use `--secret` or `--ssh`; their source files
are streamed to memory-backed storage and automatically excluded from the main
context when located below it. Continue to maintain `.dockerignore` for every
other sensitive local file.
