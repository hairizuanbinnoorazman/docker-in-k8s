# Docker build compatibility checklist

Last reviewed: July 17, 2026

Comparison baseline:

- Docker CLI `29.6.1`
- Docker Buildx `0.35.0`
- Current Docker Buildx build reference: <https://docs.docker.com/reference/cli/docker/buildx/build/>
- Docker build-context reference: <https://docs.docker.com/build/concepts/context/>

## Verification record

| Date | Feature | Unit/integration verification | Minikube verification |
| --- | --- | --- | --- |
| July 17, 2026 | `.dockerignore` and Dockerfile-specific ignore files | `go test ./...` | `test/e2e/build.sh` passed on the `dockube-build` profile with an ignored, incompressible 800 KB file, proving filtering occurred before the 700 KiB upload gate. |
| July 17, 2026 | Direct context streaming | `go test ./...` | `test/e2e/build.sh` passed on the `dockube-build` profile with an included, incompressible 800 KB file, proving the former ConfigMap limit no longer applies. |
| July 17, 2026 | BuildKit pinning and build resources | `go test ./...`, including exact image and resource assertions | `test/e2e/build.sh` passed on the `dockube-build` profile using `moby/buildkit:v0.30.0-rootless` with enforced requests and limits. |
| July 17, 2026 | Secure and authenticated registries | `go test ./...`, including registry output and Secret-volume assertions | `test/e2e/build-registry-security.sh` rejected an untrusted CA, rejected missing credentials, then pushed successfully with mounted CA and Docker config Secrets. |
| July 17, 2026 | Live logs and startup diagnostics | `go test ./...`, including Pod startup-reason assertions | `test/e2e/build.sh` observed output before process exit; `test/e2e/build-diagnostics.sh` verified missing-Secret Pod events and quota-denied Job events. |
| July 17, 2026 | Explicit push and optional timeout | `go test ./...`, including deadline assertions | `test/e2e/build.sh` and `test/e2e/build-registry-security.sh` passed with explicit `--push` and the default no-deadline behavior. |
| July 17, 2026 | Common Dockerfile controls | `go test ./...`, including exact frontend arguments and multiple-tag encoding | `test/e2e/build.sh` passed with two tags, environment build arg, target, label, `linux/amd64`, no-cache controls, and pull; the second tag was used to run the result. |
| July 17, 2026 | Persistent and registry caches | `go test ./...`, including PVC and cache argument assertions | `test/e2e/build-cache.sh` verified a cache hit across Jobs sharing a PVC and a cache hit after registry export/import between ephemeral Jobs. |
| July 17, 2026 | Build secrets | `go test ./...`, including file/env parsing, path validation, tmpfs volume, and argument redaction assertions | `test/e2e/build-secret.sh` consumed random file and environment secrets, verified their hashes in a Dockerfile secret mount, and confirmed values were absent from output. |
| July 17, 2026 | SSH private-key mounts | `go test ./...`, including source validation and argument redaction assertions | `test/e2e/build-ssh.sh` generated a temporary Ed25519 key, consumed it through `RUN --mount=type=ssh`, and confirmed its local path and value were absent from output. |
| July 17, 2026 | Progress and result metadata | `go test ./...`, including progress validation, result handoff, and image-ID parsing | `test/e2e/build-results.sh` verified plain progress, exact quiet output, valid raw JSON lines, an iid file, and BuildKit metadata copied back to the client. |
| July 17, 2026 | Multi-platform manifests | `go test ./...` | `test/e2e/build-multiplatform.sh` pushed `linux/amd64` and `linux/arm64` variants and verified both platform descriptors in the registry manifest list. |
| July 17, 2026 | Network and security controls | `go test ./...`, including frontend argument and validation tests | `test/e2e/build-network.sh` built with `--network=none` and a static host mapping, verified outbound isolation, and exercised specific rejection errors for host networking, entitlements, cgroups, loading, custom builders, policy, and legacy options. |
| July 17, 2026 | Build resource controls | `go test ./...`, including shared-memory parsing and frontend arguments | `test/e2e/build-network.sh` verified a 32 MiB `/dev/shm` inside a `RUN` step and specific errors for unsupported per-build resource and ulimit overrides. |
| July 17, 2026 | Cache backend policy | `go test ./...`, including import/export allowlists | `test/e2e/build-cache.sh` reverified PVC and registry cache hits and confirmed local cache passthrough is rejected before Job creation. |
| July 17, 2026 | Local archive/stdin contexts and external/stdin Dockerfiles | `go test ./...`, including tar extraction, traversal/special-file rejection, input limits, ambiguity rejection, and controlled Dockerfile injection | `test/e2e/build-contexts.sh` pushed builds from local and stdin tar archives plus directory contexts using external and stdin Dockerfiles. |
| July 17, 2026 | Git and verified-HTTPS remote inputs | `go test ./...`, including an in-process HTTPS transport, text/tar normalization, credential/scheme policy, and a real local Git clone | `test/e2e/build-contexts.sh` pushed Git, HTTPS tar, HTTPS text, and HTTPS remote-Dockerfile builds using a temporary trusted CA, then rejected plain HTTP. |
| July 17, 2026 | Named build contexts | `go test ./...`, including parsing, duplicate/name validation, `.dockerignore`, and exact frontend mappings | `test/e2e/build-contexts.sh` streamed a Dockerfile-free auxiliary directory and consumed it with `COPY --from=assets`. |
| July 17, 2026 | Repeatable registry and client outputs | `go test ./...`, including output validation, exact exporter arguments, and local traversal rejection | `test/e2e/build-outputs.sh` produced OCI, Docker, rootfs tar, and safely extracted local output from one untagged Job, then verified explicit registry output by fetching its manifest. |
| July 17, 2026 | Annotations, attestations, Dockerfile calls, and redacted debug | `go test ./...`, including annotation target mapping, attestation shorthands, call validation, and debug-value redaction | `test/e2e/build-advanced.sh` ran check/outline/targets, verified debug output without a random build-argument value, pushed index annotations, generated provenance and SBOM attestations, and verified the registry attestation manifest. |
| July 17, 2026 | Context filesystem and credential-source hardening | `go test ./...`, including missing/directory Dockerfiles, traversal, symlinks, unreadable/special files, archive limits, and automatic credential-source exclusion | `test/e2e/build-contexts.sh` reverified symlink streaming plus FIFO/unreadable rejection; `test/e2e/build-secret.sh` proved a secret source below the context was absent from `COPY .` while still available as a tmpfs secret mount. |
| July 17, 2026 | Generated Job contract and cleanup | `go test ./...`, including exact arguments/environment, BuildKit pin, security context, resources, volumes, registry mounts, and keep/delete behavior using a fake client | `test/e2e/build-cleanup.sh` verified zero retained Jobs after successful, failed, timed-out, and SIGTERM-canceled builds. |
| July 17, 2026 | Failure and log diagnostics | `go test ./...`, including unschedulable/image-pull states, missing/interrupted log errors, root-cause precedence, and TTY rejection | `test/e2e/build-diagnostics.sh` verified missing-volume and admission/quota events; `test/e2e/build-cleanup.sh` exercised a terminal Job failure. Interactive resize is not applicable because TTY progress is rejected. |
| July 17, 2026 | Final regression suite | `go test ./...` and `go vet ./...` passed | `make test-e2e` passed in full on the `dockube-build` Minikube profile after catching and fixing repeated-tag exporter quoting. |

## Current implementation

The repository currently implements a deliberately limited build workflow:

```console
dockube build --push -t REGISTRY/IMAGE PATH
```

Current behavior:

- Accepts filtered local directories, local/stdin tar archives, Git and
  verified-HTTPS contexts, HTTPS text Dockerfiles, and named contexts.
- Requires an explicit output: `--push` plus tags, or one or more registry,
  OCI, Docker, tar, or local `-o/--output` exporters.
- Resolves explicit local Dockerfiles from the client working directory and
  accepts stdin and verified-HTTPS Dockerfiles.
- Normalizes inputs to gzip-compressed tar data and safely extracts archives
  with traversal, special-file, and size checks.
- Applies `.dockerignore` or a Dockerfile-specific ignore file before creating
  the archive. Local `.git` data is included unless an ignore rule excludes it.
- Streams the compressed context directly into the BuildKit Pod over the
  Kubernetes exec API.
- Runs rootless BuildKit in a temporary Kubernetes Job.
- Pushes registry results or securely streams file/directory results back to
  the client.
- Uses verified TLS by default and permits insecure HTTP only through
  `--registry-insecure`.
- Deletes the Job after completion unless `--keep-build` is used.
- Supports the dockube-specific `--build-namespace`, `--timeout`, and
  `--keep-build` flags.
- Streams BuildKit logs while the Job runs.

The command now supports every item evaluated in this checklist. Options that
conflict with the socketless Kubernetes security model or lack a defined
credential/retention policy return specific errors rather than passing through.

## P0 — Correctness, security, and usability

- [x] Apply `.dockerignore` before archiving and uploading the context.
- [x] Support Dockerfile-specific ignore files such as
  `build.Dockerfile.dockerignore`.
- [x] Verify ignore-pattern behavior, including comments, negation, directory
  patterns, and precedence.
- [x] Replace the ConfigMap context transport with a scalable mechanism such as
  streaming upload, a temporary PVC, or controlled object storage.
- [x] Remove the 700 KiB development-only context limitation.
- [x] Ensure ignored files and local secrets are never stored in Kubernetes
  merely because they are present below the context directory.
- [x] Make verified TLS registry connections the default.
- [x] Remove the unconditional `registry.insecure=true` exporter option.
- [x] Add an explicit administrator-controllable opt-in for insecure registries.
- [x] Add registry authentication using Kubernetes Secret references.
- [x] Add support for private registry CA certificates without disabling TLS
  verification.
- [x] Prevent registry credentials from appearing in ConfigMaps, Job arguments,
  environment output, events, or logs.
- [x] Stream BuildKit progress and container output while the build is running.
- [x] Report Pod scheduling, admission, image-pull, and startup failures instead
  of waiting for a generic build timeout.
- [x] Pin the BuildKit image to a tested version or digest rather than using an
  unqualified mutable tag.
- [x] Configure CPU, memory, and ephemeral-storage requests and limits for build
  Jobs.
- [x] Decide and document output behavior when `--push` is not supplied. Docker
  does not push by default, while dockube currently always pushes because it has
  no local image store.
- [x] Decide whether `--timeout` remains a dockube-specific safety control or
  becomes an optional/global setting; Docker has no fixed five-minute limit.

## P1 — Common Docker build functionality

- [x] Allow repeated `-t/--tag` values.
- [x] Allow an untagged result when the selected output type supports it.
- [x] Add repeatable `--build-arg` values.
- [x] Support environment fallback for `--build-arg NAME` without an explicit
  value.
- [x] Add `--target` for selecting a multi-stage Dockerfile target.
- [x] Add repeatable `--label` values.
- [x] Add single-platform `--platform` support.
- [x] Add multi-platform manifest builds after verifying worker-platform and
  `binfmt_misc` requirements.
- [x] Add `--no-cache`.
- [x] Add `--no-cache-filter` for selected stages.
- [x] Add `--pull`.
- [x] Add persistent BuildKit cache storage instead of a new empty cache for
  every Job.
- [x] Add registry-backed `--cache-from`.
- [x] Add registry-backed `--cache-to`.
- [x] Add other cache backends only after defining credential and data-retention
  behavior.
- [x] Add `--secret` without putting secret values into the build context or a
  ConfigMap.
- [x] Add `--ssh` without copying private keys into the build context.
- [x] Add `--progress=plain`.
- [x] Add `--progress=quiet` and `-q/--quiet`.
- [x] Add `--progress=rawjson` if BuildKit status events can be streamed without
  lossy log conversion.
- [x] Add `--iidfile` and safely copy the result to the client.
- [x] Add `--metadata-file` and safely copy the result to the client.
- [x] Accept `--push` explicitly and align it with registry output semantics.

## P2 — Context and output parity

- [x] Accept local tar archives as contexts.
- [x] Accept context data from stdin (`-`).
- [x] Accept a Dockerfile from stdin with `-f -`.
- [x] Accept Git repository contexts.
- [x] Accept HTTP(S) tarball and text-file contexts.
- [x] Define safe handling for remote redirects, authentication, maximum size,
  and network policy.
- [x] Resolve local `-f/--file` paths using Docker-compatible semantics rather
  than requiring the Dockerfile to be inside the context directory.
- [x] Support remote Dockerfiles only after defining remote-input policy.
- [x] Add repeatable named `--build-context` inputs.
- [x] Add registry output through `-o/--output`.
- [x] Add OCI tar output streamed back to the client.
- [x] Add Docker image tar output streamed back to the client.
- [x] Add tar and local filesystem exporters with safe path handling.
- [x] Add repeatable output support where the BuildKit worker supports it.
- [x] Add `--annotation`.
- [x] Add `--attest`.
- [x] Add `--provenance`.
- [x] Add `--sbom`.
- [x] Add `--call=check`, `--check`, `--call=outline`, and `--call=targets`.
- [x] Add supported `--network` modes, initially `default` and `none`.
- [x] Add `--add-host` with validation appropriate to Kubernetes networking.
- [x] Evaluate `--resource` and map supported values to enforceable Kubernetes
  or BuildKit limits.
- [x] Evaluate `--shm-size`.
- [x] Evaluate `--ulimit`.
- [x] Evaluate Buildx policy files through `--policy`.
- [x] Evaluate whether `--builder` should select an administrator-defined
  Kubernetes builder profile.
- [x] Add scoped debug logging without exposing build arguments, credentials, or
  secret values.

## Explicitly reject or redefine

These options conflict with the socketless Kubernetes security model and must
not be passed through without an explicit product and administrator policy.

- [x] Reject `--allow=security.insecure` by default.
- [x] Reject `--allow=network.host` by default.
- [x] Reject device entitlements by default.
- [x] Reject `--network=host`.
- [x] Reject host device access.
- [x] Reject host cgroup selection through `--cgroup-parent`.
- [x] Reject `--load` with an explanation that dockube has no Docker Engine image
  store; offer OCI or Docker tar output when implemented.
- [x] Keep Windows legacy-builder flags and behavior out of scope.
- [x] Document every rejected option in the CLI compatibility matrix and return
  a specific error rather than silently ignoring it.

## Tests and documentation

- [x] Add archive tests for `.dockerignore` and Dockerfile-specific ignore files.
- [x] Test ignore negation and precedence behavior against Docker's pattern
  matcher.
- [x] Test missing Dockerfiles, directory Dockerfiles, path traversal, symlinks,
  unreadable files, special files, and context-size limits.
- [x] Unit-test generated Job arguments and environment variables.
- [x] Unit-test BuildKit image pinning, security context, resource limits,
  volumes, registry configuration, and cleanup policy.
- [x] Test cleanup after successful builds, failed builds, cancellation, and
  timeouts.
- [x] Test unschedulable Pods, admission rejection, image-pull failure, Job
  failure, and missing logs.
- [x] Test interrupted live log streams and terminal resize behavior if an
  interactive progress mode is added.
- [x] Add an end-to-end test using a TLS registry.
- [x] Add an end-to-end test using an authenticated registry.
- [x] Add end-to-end coverage for build arguments, target stages, multiple tags,
  secrets, cache import/export, and platforms.
- [x] Verify multi-platform images by inspecting the pushed manifest list.
- [x] Update the README compatibility matrix as each feature becomes available.
- [x] Document required build-namespace RBAC, Pod Security exceptions, quotas,
  network policy, registry access, credential setup, and retention behavior.
- [x] Treat `docker compose build` and Compose `build:` support as a separate
  follow-up project; Compose currently rejects image builds.

## Suggested delivery order

1. Context filtering and scalable context transport.
2. Secure and authenticated registry support.
3. Live logs, failure diagnostics, resource controls, and BuildKit pinning.
4. Common Dockerfile controls: build arguments, target, labels, multiple tags,
   pull, and no-cache.
5. Persistent and external caches.
6. Secrets and SSH forwarding.
7. Platforms and multi-platform manifests.
8. Additional input and output types.
9. Metadata, attestations, policy, and advanced Buildx controls.
