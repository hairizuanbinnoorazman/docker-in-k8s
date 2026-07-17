# dockube

`dockube` provides a deliberately limited Docker-like CLI backed by Kubernetes
custom resources and Pods. It never connects to a Docker, containerd, CRI, or
kubelet socket.

The default development namespace is `dockube-workloads`.

## Using it as `docker` inside a Pod

The `dockube` binary can be installed as `/usr/local/bin/docker` in a designated
developer or toolbox Pod. The CLI already supports Kubernetes in-cluster
authentication, so users can then run commands such as:

```console
docker run -d --name web nginx
docker ps
docker logs web
docker exec -it web sh
```

For example, add the binary to the toolbox image:

```dockerfile
COPY dockube /usr/local/bin/docker
```

Configure the Pod with a dedicated ServiceAccount and target namespace:

```yaml
spec:
  serviceAccountName: dockube-user
  containers:
    - name: workspace
      image: your-toolbox-with-dockube
      env:
        - name: DOCKUBE_NAMESPACE
          value: dockube-workloads
```

The `dockube-user` ServiceAccount needs namespace-scoped RBAC for:

| API resource | Required verbs | Used by |
| --- | --- | --- |
| `dockercontainers.dockube.io` | `create`, `get`, `list`, `update`, `patch`, `delete` | Lifecycle and listing commands |
| `pods` | `get` | Resolving the active Pod |
| `pods/log` | `get` | `docker logs` |
| `pods/exec` | `create` | `docker exec` |

The toolbox Pod must allow its ServiceAccount token to be mounted. This access
should only be granted to designated user-facing Pods and should be restricted
to the intended workload namespace.

Do not automatically grant the same access to workloads created by
`docker run`. Managed workload Pods deliberately set
`automountServiceAccountToken: false`, preventing them from using inherited
Kubernetes credentials to create nested workloads.

This provides Docker-like CLI syntax, not Docker Engine compatibility. Software
that requires `/var/run/docker.sock`, `DOCKER_HOST`, the Docker Engine HTTP API,
Docker plugins, or direct container-runtime access will not work.

The repository currently includes controller RBAC, but it does not yet include
the end-user `dockube-user` ServiceAccount/Role or a toolbox image containing
the CLI. Those resources must be supplied by the deployment integrating
`dockube`.

## Compatibility matrix

Legend: **Available** is implemented in the current CLI, **Partial** has
important differences or only a subset of Docker flags, and **Not available**
is not implemented.

| Docker workflow | Status | Current support and differences |
| --- | --- | --- |
| `docker run IMAGE [COMMAND] [ARG...]` | Partial | Creates a `DockerContainer` custom resource and a Pod. Foreground mode waits for termination but does not attach to the process output. |
| `docker run -d` | Available | Returns a stable 12-character logical container ID. |
| `docker run --name NAME` | Available | Assigns a stable logical container name. |
| `docker run -i`, `docker run -t`, `docker run -it` | Partial | Configures stdin/TTY on the Pod, but foreground `run` does not currently attach an interactive session. |
| Other `docker run` flags | Not available | Ports, volumes, environment variables, resources, networks, privileges, restart policies, and other Docker flags are not implemented. |
| `docker ps` | Available | Lists running managed `DockerContainer` resources. |
| `docker ps -a` | Available | Lists running and stopped/completed managed resources. Other `ps` flags are not implemented. |
| `docker logs CONTAINER` | Available | Reads logs from the current Kubernetes Pod. |
| `docker logs -f` | Available | Follows the current Pod log stream. |
| `docker logs --tail N` / `-n N` | Available | Returns the requested number of trailing lines. Other log flags are not implemented. |
| `docker exec CONTAINER COMMAND [ARG...]` | Available | Uses the Kubernetes Pod exec API and requires a running managed container. |
| `docker exec -i`, `docker exec -t`, `docker exec -it` | Available | Supports stdin and TTY streaming through the Kubernetes API. |
| `docker stop CONTAINER [...]` | Available | Deletes the active Pod and retains the logical container as stopped. Docker timeout flags are not implemented. |
| `docker start CONTAINER [...]` | Available | Creates a replacement Pod while retaining the logical container name and ID. |
| `docker restart CONTAINER [...]` | Available | Stops and recreates the Pod. The replacement can receive a new Pod UID, IP, and node. |
| `docker rm CONTAINER [...]` | Available | Deletes the logical container and its managed Pod. Force and volume flags are not implemented. |
| Container name lookup | Available | Commands accept the logical name. |
| Container ID lookup | Available | Commands accept an unambiguous prefix of the 12-character logical ID. |
| `docker create` | Not available | No create-without-starting command yet. |
| `docker inspect` | Not available | Kubernetes resources can be inspected with `kubectl` in the meantime. |
| `docker attach` | Not available | Only `exec` currently supports interactive streaming. |
| `docker kill`, `wait`, `port`, `cp`, `top`, `stats`, `events` | Not available | Not implemented. |
| `docker compose config` | Available | Uses compose-go/v2 to merge, interpolate, normalize, and validate the complete project without cluster access. |
| `docker compose up -d` | Partial | Idempotently creates one logical workload and ClusterIP Service per service, plus ConfigMaps and PVCs. Replicas are limited to one. |
| `docker compose ps`, `logs` | Available | Lists project workloads and reads current Pod logs. `logs -f` is best used with one service. |
| `docker compose stop`, `start`, `restart` | Available | Operates on every service in the project. Replacement Pods receive new UIDs and IPs. |
| `docker compose down` | Available | Removes workloads, Services, and generated ConfigMaps. PVCs are retained unless `--volumes` is supplied. |
| `docker build [OPTIONS] PATH` | Partial | Streams filtered local, tar, stdin, Git, HTTPS, and named contexts into a short-lived rootless BuildKit Kubernetes Job. Supports registry/OCI/Docker/tar/local outputs, multiple tags and outputs, common Dockerfile/cache controls, multi-platform manifests, tmpfs secrets and SSH keys, annotations and attestations, frontend calls, result metadata, network isolation, and redacted debug diagnostics. Docker Engine loading, privileged/host controls, interactive progress, binary stdout output, and several cloud cache backends remain intentionally unavailable. |
| Docker Engine API and socket clients | Not available | There is intentionally no Docker socket or Docker Engine API compatibility. |
| Host bind mounts, privileged mode, host namespaces, and devices | Not available | Intentionally excluded from the security model. |

Build flags that require host privileges or a Docker Engine are rejected with a
specific error. `--network=host`, every `--allow` entitlement (including
`security.insecure`, `network.host`, and devices), `--cgroup-parent`, and
`--add-host=host:host-gateway` conflict with the socketless Kubernetes security
model. `--load` has no destination because dockube has no Docker image store.
Only the built-in/default builder is currently selectable; `--policy` is
reserved until an administrator-controlled policy design exists. Windows and
legacy-builder flags `--isolation`, `--security-opt`, and `--squash` are out of
scope. Build Job CPU, memory, and ephemeral-storage limits are
administrator-defined, so `--resource` is rejected; node/runtime process limits
cannot safely be changed per build, so `--ulimit` is also rejected. BuildKit's
per-`RUN` shared memory can be set with `--shm-size`. None of these options is
silently ignored.

Cache import supports the registry backend. Cache export supports registry and
inline backends. Local, GitHub Actions, S3, Azure Blob, and other cache types
are rejected because dockube has not defined their credential exposure, data
retention, or cross-tenant isolation behavior.

Build contexts may be local directories or uncompressed/gzip-compressed tar
archives. A `-f/--file` supplied explicitly is resolved from the client working
directory and may be outside the context; dockube injects it under a controlled
name without adding neighboring files. Archive extraction rejects traversal,
devices, FIFOs, and other special entries and is capped at 2 GiB.
`PATH=-` accepts a tar archive from stdin, while `-f -` reads a Dockerfile of up
to 16 MiB from stdin for a non-stdin context. The two inputs cannot share stdin.
Remote contexts support verified-HTTPS tar archives and UTF-8 text Dockerfiles,
and Git contexts support `git+https://`, HTTPS URLs ending in `.git`, and local
`file://` repositories. Remote Dockerfiles are also accepted over verified
HTTPS. Network inputs reject URL credentials and plain HTTP, follow at most five
verified-HTTPS redirects, allow at most 2 GiB for a context and 16 MiB for a
Dockerfile, and are downloaded on the client before filtered streaming to the
cluster. HTTPS trust follows the client's system trust configuration.
Repeatable `--build-context NAME=PATH_OR_URL` inputs use the same filtering and
remote-input policy and are streamed into separate private directories for
Dockerfile named-context references.

Repeatable `-o/--output` accepts `registry`, `oci`, `docker`, `tar`, and `local`
exporters. Registry outputs are pushed; the other types are streamed from the
BuildKit Pod to an atomic client file or a newly created local directory. Local
directory extraction rejects traversal, links, devices, and other special
entries. Non-registry outputs do not require a tag or `--push`. Binary stdout
(`dest=-`) remains rejected until progress and result streams are separated.

Files selected as `--secret` or private-key `--ssh` sources are automatically
excluded from the primary context archive when they reside below the context,
even without an ignore rule. Other sensitive local files remain subject to
normal Docker context semantics and must be listed in `.dockerignore`.

Image outputs accept repeatable `--annotation` values, including manifest/index
and platform qualifiers. `--attest` supports BuildKit provenance and SBOM
attestations, with `--provenance` and `--sbom` shorthands. Dockerfile frontend
subrequests are available through `--call=check|outline|targets`; `--check` is
the check shorthand. These run in the same constrained rootless BuildKit Job.
`--debug` emits only scoped lifecycle metadata (generated Job/namespace,
BuildKit pin, feature counts, cleanup policy, and whether a timeout is enabled).
It deliberately omits tags, paths, argument values, secret identifiers and
values, SSH identifiers and keys, and registry credential details.
Interactive/TTY progress is intentionally unavailable; `--progress=tty` is
rejected. Plain and raw-JSON log streams report interruption or missing-log
errors, while an actual Job failure retains precedence as the root cause.

## Tested environments

The CLI and controller have been tested on:

- **Minikube**
- **GKE Standard**, Kubernetes `v1.35.5-gke.1000000`, on June 21, 2026

The GKE run covered controller deployment and RBAC, Docker-like lifecycle
commands, logs and exec, restricted Pod security settings, Compose DNS,
ConfigMaps, Secrets, CSI-backed persistent volumes, idempotent updates,
stop/start/restart, and cleanup.

See [docs/gke-validation.md](docs/gke-validation.md) for the test procedure,
sanitized evidence, discovered issues, and GKE deployment requirements. Other
managed Kubernetes services and Kubernetes versions have not yet been
validated, so compatibility with them should not be assumed.

See [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md) for architecture,
compatibility boundaries, security constraints, and the remaining task list.
See [docs/build-operations.md](docs/build-operations.md) for build-namespace
RBAC, Pod Security exceptions, quota sizing, network policy, registry
credentials, cache isolation, and retention requirements.

## Compose compatibility

`dockube compose` uses `github.com/compose-spec/compose-go/v2` and validates the
whole normalized project before creating or updating Kubernetes resources.
Compose services must reference prebuilt `image:` values. `build:` is rejected
explicitly and remains a separate follow-up project; `dockube build` is not
implicitly invoked by `compose up`.
Project names, ownership labels, workload names, Services, ConfigMaps, and PVCs
are deterministic. Repeated `compose up -d` calls update existing resources and
remove stale project workloads without creating duplicates.

| Compose field | Support | Kubernetes mapping or restriction |
| --- | --- | --- |
| `image` | Supported | Required; image builds are not performed. |
| `entrypoint`, `command` | Supported | Container command and arguments. |
| `environment`, `env_file` | Supported | Resolved by compose-go before apply; values are stored in the workload spec. |
| `working_dir`, `stdin_open`, `tty` | Supported | Container fields. |
| `expose`, `ports` | Partial | ClusterIP Service ports only. Published ports become Service ports; no host binding or public LoadBalancer is created. |
| `deploy.resources` CPU/memory requests and limits | Supported | Kubernetes resource requests and limits. |
| `healthcheck` with `CMD` or `CMD-SHELL` | Supported | Readiness and liveness exec probes; `start_period` also creates a startup probe. |
| Named volumes | Supported | 1 GiB `ReadWriteOnce` PVCs by default. External volumes reference existing PVCs. PVCs survive `down`; `down --volumes` deletes managed PVCs. |
| `configs` from `file`, `content`, or `environment` | Supported | Generated ConfigMaps mounted at the requested target. |
| External `secrets` | Supported | References an existing Kubernetes Secret. The secret must contain a key matching the Compose secret source name. Secret data is never copied into a dockube object or status. |
| `depends_on: service_started` | Partial | Dependency validity is checked, but Kubernetes scheduling remains concurrent; applications must retry DNS/connections. |
| Default network | Partial | Every service gets a Kubernetes Service and service-name DNS. Custom network semantics and aliases are not implemented. |
| Replicas | Partial | Exactly one replica per service. |
| `build`, host/anonymous/tmpfs mounts, `privileged`, devices, host networking/PID/IPC/UTS, capabilities, sysctls, custom logging, custom DNS, and other non-listed service fields | Rejected | Validation fails before cluster mutation; there is no silent degradation. |

The identity running the Compose CLI additionally needs namespace-scoped access
to `services`, `configmaps`, and `persistentvolumeclaims`, and `get` access to
referenced `secrets`. The controller itself does not need Secret access: Pods
reference Secrets directly and kubelet performs the mount.

## Building images on Minikube

By default, the development install creates a private registry in a separate
Pod, backed by a 2 GiB PVC. It is available inside the cluster as
`dockube-registry.dockube-system.svc:5000` and on each node at port `30500`.
BuildKit `v0.30.0-rootless` runs with CPU, memory, and ephemeral-storage
requests and limits only for the duration of each build; no Docker or
container-runtime socket is mounted.

The bundled registry is controlled by `DOCKUBE_BUNDLED_REGISTRY` and defaults
to `true`:

```console
# Default: install the registry Deployment, Service, and PVC.
make install

# Use an externally managed registry instead.
make install DOCKUBE_BUNDLED_REGISTRY=false
```

Disabling the bundled registry does not configure or validate an external
registry. Pass that registry in the build tag, and separately configure its
TLS credentials or node-runtime trust as required. `make uninstall-registry`
removes the bundled registry and its PVC, including all stored images.

Minikube's container runtime must trust the development registry's plain HTTP
endpoint. For a fresh profile, start Minikube once to establish its node IP,
then restart it with that exact registry endpoint:

```console
minikube start
REGISTRY="$(minikube ip):30500"
minikube stop
minikube start --insecure-registry="$REGISTRY"
make deploy-dev
make build

./bin/dockube --namespace dockube-workloads build --push --registry-insecure -t "$REGISTRY/dockube-example:dev" .
./bin/dockube --namespace dockube-workloads run -d --name built-example "$REGISTRY/dockube-example:dev"
```

The identity invoking `build` needs permission to create/get/delete Jobs, list
Pods, list Events, create `pods/exec` sessions, and get `pods/log` in
`dockube-system`. Events are used to report admission, scheduling, mount, and
image-pull failures without waiting for the overall build timeout.
Rootless BuildKit needs an unconfined seccomp/AppArmor profile and its setuid
UID-mapping helper, so the build namespace cannot enforce the restricted Pod
Security profile. The build Pod still runs its main process as UID 1000, mounts
no host paths, and receives no Kubernetes service-account token.

The filtered context is gzip-compressed and streamed directly into an `emptyDir`
mounted by the BuildKit Pod; it is not stored in a ConfigMap or Kubernetes etcd.
Add local secrets to `.dockerignore` so they are removed before upload, and do
not use context files to pass build secrets. The Job and its ephemeral context
are removed after the build unless `--keep-build` is specified.

Verified TLS is the default for external registries. Use
`--registry-secret NAME` to mount a `kubernetes.io/dockerconfigjson` Secret as
the BuildKit Docker configuration, and `--registry-ca-secret NAME` to mount a
Secret whose `ca.crt` key contains a private registry CA. Credential and CA
values are mounted directly into the build Pod and are not copied into Job
arguments, environment values, ConfigMaps, events, or build logs. The bundled
development registry uses plain HTTP and therefore requires the explicit
`--registry-insecure` flag shown above.

Build secrets supplied with `--secret` and SSH private keys supplied with
`--ssh ID=PATH` are streamed separately into a memory-backed volume after the
context upload. Their values and local paths are not placed in the context, Job
arguments, Pod environment, ConfigMaps, or logs. SSH agent sockets are not yet
forwarded; pass a private-key file explicitly.

Repeat `--platform` or pass a comma-separated list to push a multi-platform
manifest. Dockerfiles that only select or copy platform-specific content do not
need CPU emulation. A `RUN` instruction for a non-native architecture requires
the cluster node to have a matching `binfmt_misc` emulator; dockube does not
install host-level emulators and BuildKit will fail the affected platform when
one is unavailable.
