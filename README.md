# dockube

`dockube` provides a deliberately limited Docker-like CLI backed by Kubernetes
custom resources and Pods. It never connects to a Docker, containerd, CRI, or
kubelet socket.

Current implementation slice:

- `dockube run -d --name NAME IMAGE [COMMAND] [ARG...]`
- `dockube ps [-a]`
- `dockube logs [-f] [--tail N] CONTAINER`
- `dockube exec [-i] [-t] CONTAINER COMMAND [ARG...]`
- `dockube stop|start|restart CONTAINER`
- `dockube rm CONTAINER`

The default development namespace is `dockube-workloads`.

See [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md) for architecture,
compatibility boundaries, security constraints, and the remaining task list.
