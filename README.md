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
| `docker compose ...` | Not available | Compose parsing and reconciliation are planned but not implemented. |
| `docker build` / BuildKit | Not available | Image builds are outside the current implementation. |
| Docker Engine API and socket clients | Not available | There is intentionally no Docker socket or Docker Engine API compatibility. |
| Host bind mounts, privileged mode, host namespaces, and devices | Not available | Intentionally excluded from the security model. |

## Tested environments

The CLI and controller have currently been tested only on **Minikube**. Other
Kubernetes distributions and managed Kubernetes services have not yet been
validated, so compatibility with them should not be assumed.

See [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md) for architecture,
compatibility boundaries, security constraints, and the remaining task list.
