# Docker-compatible workflows on Kubernetes

Status: proposed implementation plan  
Working name: `dockube` (installable or aliased as `docker`)  
Primary goal: provide familiar `docker` and `docker compose` workflows while running workloads as Kubernetes resources in an isolated namespace, without mounting a container runtime socket.

## 1. Executive decision

Build a Kubernetes-native CLI and controller, not a kubelet client and not a container-runtime proxy.

The CLI should authenticate to the Kubernetes **API server** using the standard in-cluster service account or kubeconfig mechanism. The API server then applies authentication, namespace-scoped RBAC, admission policy, audit logging, and resource quotas before the scheduler and kubelets act on the workload.

Use a small custom resource as the durable representation of a Docker-like logical container. A controller reconciles each resource to a Pod and related resources. This is necessary because a Kubernetes Pod cannot be restarted after it reaches a terminal phase, completed Pods may be garbage-collected, and Pod identity alone cannot accurately implement commands such as `docker start`, `docker ps -a`, and stable container names.

Do not claim complete Docker compatibility. Docker and Kubernetes have materially different storage, networking, lifecycle, build, privilege, and restart semantics. Publish a tested compatibility matrix and fail explicitly for unsupported flags.

## 2. Target user experience

Example:

```console
$ docker run --name web -d -p 8080:80 nginx:1.27
4db7a0f3c2e1

$ docker ps -a
CONTAINER ID   IMAGE        COMMAND   CREATED          STATUS          PORTS          NAMES
4db7a0f3c2e1   nginx:1.27             12 seconds ago   Up 10 seconds   8080->80/tcp   web

$ docker logs -f web
$ docker exec -it web sh
$ docker stop web
$ docker start web
$ docker rm web

$ docker compose up -d
$ docker compose ps
$ docker compose logs -f
$ docker compose down
```

All managed resources are created in a configured target namespace, for example `docker-workloads-alice`. The CLI only displays resources bearing the project's ownership labels; it must not present unrelated namespace Pods as Docker containers.

## 3. Scope and compatibility contract

### MVP commands

- `docker run`
- `docker create`
- `docker ps` and `docker ps -a`
- `docker inspect`
- `docker logs` and `docker logs -f`
- `docker exec`
- `docker attach` for a supported interactive configuration
- `docker stop`, `start`, `restart`, `kill`, and `wait`
- `docker rm`
- `docker port`
- `docker compose config`
- `docker compose up`, `ps`, `logs`, `stop`, `start`, `restart`, and `down`

### Follow-up commands

- `docker cp`
- `docker top`
- `docker stats`
- `docker events`
- `docker compose exec`, `run`, `pull`, and `images`
- Registry authentication and credential helpers
- Compose profiles, health checks, dependencies, configs, and secrets

### Explicitly out of MVP

- `docker build`, BuildKit, and build cache management
- Docker Swarm commands
- Docker plugins
- Direct container-runtime or CRI operations
- Host bind mounts such as `-v /host/path:/container/path`
- `--privileged`, host PID/IPC/network, host devices, arbitrary capabilities, and runtime socket mounts
- Exact Docker bridge networking, static container IPs, iptables behavior, and local host-port semantics
- Exact byte-for-byte Docker Engine API compatibility
- Windows containers

Unsupported behavior must return a specific error with a Kubernetes-native alternative where one exists.

## 4. Architecture

```text
docker-compatible CLI
  |
  | Kubernetes client protocol (kubeconfig or service account)
  v
Kubernetes API server
  |-- authentication / authorization / admission / audit
  |
  +--> DockerContainer CRs ----> dockube controller ----> Pods
  |                                      |-------------> Services
  |                                      |-------------> PVCs
  |
  +--> DockerComposeProject CRs -> controller ----------> DockerContainer CRs
```

### Components

1. **CLI**
   - Prefer Go for Kubernetes client support, streaming exec/log APIs, static binaries, and use of the Compose parser.
   - Own the command parser; do not shell out to `kubectl`.
   - Support `DOCKUBE_NAMESPACE`, a config file, and `--namespace`.
   - An administrator may install or alias it as `docker`. During development, keep the binary name `dockube` to avoid accidental use against a real Docker installation.

2. **`DockerContainer` custom resource**
   - Stores stable logical identity and desired lifecycle independently of Pod replacement.
   - Records normalized user intent rather than copying the complete Pod API.
   - Tracks current and previous Pod references, effective configuration, state, exit code, timestamps, and human-readable reason.

3. **Controller**
   - Converts approved `DockerContainer` intent into a Pod and optional Service/PVC.
   - Reconciles desired lifecycle (`running`, `stopped`, or `removed`) idempotently.
   - Recreates a Pod for `start` or restart policy while preserving logical container identity.
   - Uses owner references, finalizers where required, deterministic labels, and status conditions.

4. **Compose translator**
   - Parses the current Compose Specification through a maintained Compose library.
   - Produces a normalized plan, validates unsupported fields before mutation, and applies project resources.
   - Represents each Compose service as one logical service with one or more replicas. Do not model a replicated Compose service as a single Docker container.

### Proposed custom resource shape

```yaml
apiVersion: dockube.io/v1alpha1
kind: DockerContainer
metadata:
  name: web
spec:
  desiredState: Running
  image: nginx:1.27
  command: []
  args: []
  env: []
  workingDir: ""
  stdin: false
  tty: false
  restartPolicy: "no"
  ports:
    - containerPort: 80
      protocol: TCP
      publish:
        mode: PortForward
        localPort: 8080
  mounts: []
  resources: {}
  securityProfile: restricted
status:
  containerID: 4db7a0f3c2e1
  phase: Running
  currentPodName: dockube-web-a1b2c
  restartCount: 0
  exitCode: null
  createdAt: null
  startedAt: null
  finishedAt: null
  conditions: []
```

The short `containerID` is a display identifier derived from the custom resource UID. Commands must accept an unambiguous ID prefix or name.

## 5. Command-to-Kubernetes behavior

| Docker behavior | Kubernetes implementation | Important difference |
|---|---|---|
| `run` / `create` | Create `DockerContainer`; controller creates Pod on `Running` | Scheduling is asynchronous |
| `ps -a` | List managed custom resources and status | History does not depend on retaining completed Pods |
| `logs` | Read current or previous Pod logs | Cluster log retention still applies |
| `exec` | Kubernetes `pods/exec` subresource | Requires a running Pod and RBAC permission |
| `attach` | Kubernetes `pods/attach` subresource | TTY/stdin must have been configured appropriately |
| `stop` | Set desired state to `Stopped`; terminate current Pod | The stopped Pod cannot later restart |
| `start` | Set desired state to `Running`; create a replacement Pod | New Pod UID/IP; logical container ID remains stable |
| `restart` | Controlled stop followed by replacement Pod | No guarantee of the same node or IP |
| `rm` | Delete the custom resource and owned resources | PVC retention must be explicit |
| `-p` | Service, port-forward session, or Gateway/Ingress mode | There is no universal Docker host port |
| named volume | PVC | Access mode and topology depend on StorageClass |
| Compose network | Namespace DNS, Services, and optional NetworkPolicies | Not a Docker bridge network |
| restart policy | Controller reconciliation | Backoff and failure semantics differ from Docker |

For `-p`, select one product contract for MVP:

- Default recommendation: create a Service and have the foreground CLI maintain a local port-forward. This resembles local `docker run -p` but only lasts while the forwarding process is alive.
- For detached, cluster-reachable exposure, require an explicit `--publish-mode=service` and Service type.
- Do not silently create public LoadBalancers.

## 6. Security model

Removing the Docker socket is a meaningful improvement because the workload no longer receives direct control of the node's container runtime. It does **not** make arbitrary container execution safe by itself.

Required controls:

- A dedicated namespace per user, team, or trust boundary.
- Namespace-scoped Role and RoleBinding with least privilege.
- The CLI identity may manage only project custom resources and use the required streaming subresources.
- The controller has permission to create Pods, Services, ConfigMaps, Secrets references, and PVCs only in opted-in workload namespaces.
- Pod Security Admission set to `restricted`, with exceptions handled as an explicit administrator policy rather than CLI flags.
- Default `runAsNonRoot`, seccomp `RuntimeDefault`, dropped capabilities, no privilege escalation, read-only service-account token behavior, and no host namespaces or host paths.
- Do not automount a Kubernetes API token into launched workloads unless explicitly requested and authorized.
- ResourceQuota, LimitRange, object-count limits, and maximum execution duration.
- Default-deny NetworkPolicy with an explicit product decision for DNS, registry, and egress access.
- Image policy support: allowed registries, digest pinning option, signature verification integration, and admission-policy compatibility.
- Kubernetes audit logging plus CLI correlation annotations.
- Secret values must not be copied into status, labels, events, or CLI debug logs.
- Validate all translated Pod fields server-side and in the controller; never treat the CLI as a security boundary.

Threats that remain include malicious images, kernel/container-runtime escapes, abuse of permitted network access, resource exhaustion, stolen Kubernetes credentials, vulnerable admission/controller code, and data exposure through PVCs or logs.

## 7. Compose mapping

Use the Compose Specification as input, but define a support profile.

Initial mappings:

- `services`: `DockerContainer` resources for one replica; a service-level resource or Deployment-like reconciliation for multiple replicas.
- `image`, `command`, `entrypoint`, `environment`, `env_file`, `working_dir`, `user`: normalized container fields, subject to policy.
- `ports`: Service plus selected publish mode.
- Named `volumes`: PVCs. Anonymous volumes receive generated PVCs and an explicit cleanup policy.
- `configs`: ConfigMaps.
- `secrets`: references to pre-existing Kubernetes Secrets by default; avoid placing secret material in the Compose project resource.
- Service-name discovery: ClusterIP Services and Kubernetes DNS.
- `healthcheck`: readiness/liveness/startup probes where conversion is safe.
- `deploy.resources`: requests and limits.
- `depends_on`: startup ordering assistance only; never imply application readiness unless a health condition is available.

Reject or warn on fields whose semantics cannot be preserved, including `build`, host bind mounts, `network_mode: host`, `pid: host`, `privileged`, devices, unsupported sysctls, and Docker-specific networking controls.

Before `compose up`, display or expose through `--dry-run` the Kubernetes resources, policy warnings, PVC retention behavior, and externally reachable endpoints.

## 8. Delivery phases and task list

### Phase 0 — Product contract and spike

- [ ] Select a project and API group name; reserve CLI/config environment variable names.
- [ ] Write the compatibility matrix for every accepted command and flag.
- [ ] Define supported Kubernetes versions and required cluster features.
- [ ] Decide namespace tenancy: per user, per team, or caller-selected from an allow-list.
- [ ] Decide whether the CLI creates namespaces or requires pre-provisioning; recommend pre-provisioning.
- [ ] Prototype Pod create, watch, logs, exec, attach, and delete against a disposable cluster.
- [ ] Measure API behavior for TTY resize, disconnects, cancellation, Pod eviction, and node loss.
- [ ] Decide the MVP port-publishing contract.
- [ ] Record architecture decisions for CRD/controller use, stable identity, volume retention, and Compose replica mapping.
- [ ] Define success criteria and non-goals in the README.

Exit criteria: the team has a reviewed compatibility contract and streaming spike, with no unresolved lifecycle or namespace-security assumptions.

### Phase 1 — Repository and API foundation

- [ ] Initialize Go module, CLI package, controller package, API package, and integration-test layout.
- [ ] Add linting, formatting, unit tests, generated-code checks, and reproducible builds.
- [ ] Define `DockerContainer` `v1alpha1` types, schema validation, printer columns, and status conditions.
- [ ] Generate CRDs and typed clients.
- [ ] Add Helm or Kustomize installation manifests.
- [ ] Add controller ServiceAccount, namespace-scoped Roles, and RoleBindings.
- [ ] Add Pod Security Admission labels, ResourceQuota, LimitRange, and baseline NetworkPolicy examples.
- [ ] Implement configuration loading from flags, environment, config file, kubeconfig, and in-cluster credentials.
- [ ] Add a server capability/version preflight command.
- [ ] Add ownership labels and annotations as versioned constants.

Exit criteria: install/uninstall works on a clean test cluster and API conformance tests pass.

### Phase 2 — Lifecycle controller

- [ ] Implement idempotent reconciliation for create, running, stopped, failed, and deletion states.
- [ ] Generate restricted Pod specs from normalized intent.
- [ ] Implement image pull policy and image-pull-secret references.
- [ ] Track current/previous Pod and propagate useful status and events.
- [ ] Preserve logical identity across replacement Pods.
- [ ] Implement graceful stop timeout and forced termination.
- [ ] Implement Docker-like restart policy translation with bounded backoff.
- [ ] Implement Service creation for declared ports.
- [ ] Implement PVC creation/reference and explicit retain/delete behavior.
- [ ] Handle eviction, node loss, image pull failure, unschedulable Pods, and controller restart.
- [ ] Add finalizer timeout and recovery behavior so a failed controller cannot permanently wedge deletion.
- [ ] Add unit, envtest, and cluster integration tests for every state transition.

Exit criteria: lifecycle tests prove stable logical identity and deterministic cleanup under failures.

### Phase 3 — Core CLI

- [ ] Implement Docker-like global flags, name/ID resolution, output conventions, and exit codes.
- [ ] Implement `create` and `run`.
- [ ] Implement `ps` and `ps -a`, including filter and format support for documented fields.
- [ ] Implement `inspect` with a documented compatibility schema.
- [ ] Implement `logs`, follow, timestamps, tail, and since.
- [ ] Implement `exec` with stdin, TTY, resize, cancellation, and remote exit-code handling.
- [ ] Implement supported `attach` behavior and document its limitations.
- [ ] Implement `stop`, `start`, `restart`, `kill`, `wait`, and `rm`.
- [ ] Implement `port` and local port-forward lifecycle management.
- [ ] Add `--dry-run`, `--output`, verbose diagnostics, and correlation IDs.
- [ ] Reject every unsupported Docker flag rather than ignoring it.
- [ ] Add golden tests for table output and parser compatibility.
- [ ] Add end-to-end tests using representative public images, including minimal/distroless images.

Exit criteria: the documented MVP single-container workflow passes end-to-end without runtime socket access or cluster-admin credentials.

### Phase 4 — Compose

- [ ] Integrate a maintained Compose Specification parser.
- [ ] Implement interpolation, merge/include behavior selected for the support profile, and `compose config`.
- [ ] Build a normalized Compose-to-project intermediate representation.
- [ ] Validate all services before applying any resource.
- [ ] Implement project naming, ownership, labels, and deterministic resource names.
- [ ] Map services, commands, environment, ports, configs, secret references, resources, probes, and named volumes.
- [ ] Implement `compose up -d`, `ps`, `logs`, `stop`, `start`, `restart`, and `down`.
- [ ] Define replica behavior and aggregate status/output.
- [ ] Implement dependency handling without promising strict startup ordering.
- [ ] Add diff/apply behavior and rollback or clear partial-failure reporting.
- [ ] Implement `down --volumes` and safe default PVC retention.
- [ ] Add fixture tests from small, medium, stateful, and intentionally unsupported Compose files.
- [ ] Publish the Compose field compatibility matrix.

Exit criteria: supported Compose fixtures have repeatable create/update/down behavior and no silent semantic degradation.

### Phase 5 — Hardening and operations

- [ ] Threat-model CLI credentials, controller compromise, malicious images, streaming APIs, PVCs, and egress.
- [ ] Add admission-policy test suites for restricted namespaces.
- [ ] Test RBAC with `kubectl auth can-i` equivalents and negative cases.
- [ ] Add per-user/project quotas and maximum retained history.
- [ ] Add metrics for reconciliations, failures, API latency, running workloads, and quota rejection.
- [ ] Add structured logs without environment/secret leakage.
- [ ] Define controller high availability, leader election, disruption budget, and upgrade process.
- [ ] Add CRD conversion/versioning and backup/restore guidance before beta.
- [ ] Test Kubernetes minor-version compatibility and multiple container runtimes.
- [ ] Run dependency, image, manifest, and Go security scanning.
- [ ] Conduct a third-party security review before allowing untrusted users.

Exit criteria: documented least-privilege installation passes security, upgrade, recovery, and supported-version tests.

### Phase 6 — Distribution and adoption

- [ ] Publish signed CLI binaries and controller images with SBOMs and provenance.
- [ ] Provide installation, namespace onboarding, policy, and troubleshooting guides.
- [ ] Provide shell completion and an opt-in `docker` alias/wrapper.
- [ ] Add a migration guide explaining semantic differences from Docker.
- [ ] Add compatibility telemetry only if explicitly enabled and privacy-reviewed.
- [ ] Define alpha, beta, and stable API/CLI guarantees.
- [ ] Establish issue templates that require command, flags, Kubernetes version, runtime, and policy details.

## 9. Verification strategy

Minimum automated test layers:

- Parser and normalization unit tests.
- Controller reconciliation tests with fake error injection.
- API/schema tests using Kubernetes envtest.
- End-to-end tests on kind or another disposable conformant cluster.
- Negative security tests proving forbidden host access and privilege settings are rejected.
- RBAC tests using a real restricted service account.
- Streaming tests for logs/exec/attach across disconnect and timeout cases.
- Chaos tests for controller restart, Pod eviction, node loss, and API conflicts.
- Golden compatibility tests comparing documented CLI output, filters, and exit codes.
- Compose fixture tests covering updates, dependency failures, PVC retention, and cleanup.

Release gates:

- No command silently ignores an accepted flag.
- No normal user workflow requires cluster-admin.
- No launched workload receives controller or CLI credentials by default.
- No hostPath, privileged container, host namespace, or runtime socket is generated by supported inputs.
- All created objects are attributable to caller, project, CLI version, and logical container without storing secrets.
- Deletion and failed reconciliation cannot leave unbounded resources.

## 10. Design review and alternatives

### Finding 1: use the API server, not the kubelet API

The original direction is sound about avoiding the Docker socket, but “everything flows through the kubelet API” should be changed. Kubelets expose node-level operations and communicate with the container runtime through CRI. They are not the correct tenant-facing control plane. The Kubernetes API server is the stable, policy-enforced entry point.

Recommendation: use standard Kubernetes client libraries against the API server exclusively. Never connect to CRI sockets or kubelet endpoints.

### Finding 2: a thin Pod wrapper is insufficient for `ps -a` and restart

Using labels on Pods would create a quick prototype, but terminal Pods are immutable and may disappear. `docker start` would necessarily create a new Pod, while a Docker container normally retains one engine object.

Recommendation: retain the `DockerContainer` custom resource as the logical object and treat Pods as replaceable executions. For a very small proof of concept, labels-only Pods are acceptable, but that implementation should not establish the public lifecycle contract.

### Finding 3: “the whole Docker CLI” is not a bounded initial goal

Docker commands cover images, builds, networks, volumes, plugins, contexts, registries, Swarm, and an Engine API accumulated over many years. Compose adds a broad specification whose fields often depend on Docker networking and storage behavior.

Recommendation: define an intentionally compatible subset and a feature matrix. Use familiar syntax where semantics are defensible; emit hard errors elsewhere. Treat build support as a separate project using a dedicated unprivileged/isolated builder.

### Finding 4: an Engine API compatibility gateway is an alternative, not the recommended MVP

An HTTP service implementing Docker Engine API endpoints would allow the stock Docker CLI and Compose plugin to connect through `DOCKER_HOST`. This maximizes parser-level compatibility and avoids rebuilding all client-side command handling.

It also creates a large, versioned API emulation commitment, requires translating stateful Docker API behavior to Kubernetes, centralizes credentials and authorization, and still cannot preserve many semantics. The stock client may call undocumented or unexpected endpoint combinations.

Recommendation: start with a purpose-built CLI. Reconsider an Engine API facade only after measuring demand for third-party Docker API clients, and expose it as an optional compatibility layer over the same custom resources.

### Finding 5: direct CLI-to-Pod access is simpler but weaker as a product architecture

A controllerless CLI has fewer installed components and is useful for a proof of concept. However, restart policies stop functioning when the CLI exits, stable history is difficult, validation can be bypassed by older clients, and multi-object Compose updates are harder to reconcile.

Recommendation: use direct API streaming for logs/exec/attach, but use a controller and custom resources for durable lifecycle.

### Finding 6: Kubernetes Jobs are not a universal replacement for Docker containers

Jobs are appropriate for run-to-completion workloads and provide retry/completion tracking, but long-running services, interactive sessions, and Docker-style stop/start do not fit Job semantics.

Recommendation: generate Pods through the logical-container controller for MVP. Add an explicit `--workload=job` mode later for batch behavior rather than inferring it from commands.

### Finding 7: Compose should not automatically become one Pod

Putting all Compose services into one Pod gives shared localhost and lifecycle, but breaks independent scaling, restart, resource, and service identity expectations. Mapping every service directly to a Deployment also loses Docker-like one-off lifecycle and creates another abstraction mismatch.

Recommendation: map each Compose service to a service-level logical resource; use separate Pods and Services. Introduce Deployments/StatefulSets only where the chosen scaling and persistence contract requires them.

### Finding 8: security improves, but authorization moves rather than disappears

The Docker socket is commonly equivalent to strong node control. Removing it reduces that risk. A Kubernetes credential capable of creating arbitrary privileged Pods can still lead to node compromise.

Recommendation: make the restricted workload profile and namespace-scoped RBAC non-optional defaults. Treat privilege exceptions, public exposure, host access, and API token mounting as administrator-controlled policy.

### Finding 9: local filesystem and port behavior need explicit product choices

Docker users expect bind mounts and `localhost:hostPort`. A workload scheduled anywhere in a cluster cannot see the caller's filesystem or automatically bind a port on the caller's machine.

Recommendation: reject bind mounts initially; later provide an explicit upload/sync feature with size, ownership, and secret-handling controls. Use local port-forwarding for development and explicit Services/Gateway resources for cluster exposure.

## 11. Final recommendation

Proceed with a two-stage implementation:

1. Build a short proof of concept for `run`, `ps -a`, `logs`, `exec`, `stop`, `start`, and `rm` using the proposed custom resource and controller.
2. Freeze the compatibility contract only after validating lifecycle, streaming, namespace isolation, port-forwarding, and restricted admission behavior in a real cluster.

Do not begin Compose implementation until the single-container lifecycle is stable. Compose multiplies every unresolved decision around identity, networking, storage, updates, and cleanup.

## 12. Reference material

- [Kubernetes RBAC authorization](https://kubernetes.io/docs/reference/access-authn-authz/rbac/)
- [Kubernetes Pod Security Standards](https://kubernetes.io/docs/concepts/security/pod-security-standards/)
- [Kubernetes Jobs](https://kubernetes.io/docs/concepts/workloads/controllers/job/)
- [Kubernetes Container Runtime Interface](https://kubernetes.io/docs/concepts/containers/cri/)
- [Docker Compose file reference](https://docs.docker.com/reference/compose-file/)
- [Docker Compose Bridge](https://docs.docker.com/compose/bridge/)
