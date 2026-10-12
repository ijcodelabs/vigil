# Testing against the real thing

`go test ./...` uses stand-ins for the stores, Docker and journalctl. The
notes below are for live runs. They describe the original developer's Mac;
adapt the names.

## Stores and the server

There is no `docker` CLI on that machine; Podman is used, with a dedicated
machine so that other work is not disturbed:

    podman machine start vigil-test
    export CONTAINER_CONNECTION=vigil-test
    podman run -d --name vigil-vm -p 127.0.0.1:18428:8428 docker.io/victoriametrics/victoria-metrics:v1.153.0
    podman run -d --name vigil-vl -p 127.0.0.1:19428:9428 docker.io/victoriametrics/victoria-logs:v1.53.0

Then run `bin/vigil server` and `bin/vigil agent` natively against them.
Only one Podman machine runs at a time. The machine's Docker-compatible
socket (`podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}'`)
works as the agent's `docker.socket`.

`deploy/docker-compose.yml` and the Dockerfile were verified with
`podman-compose` on that machine.

VictoriaMetrics hides the most recent 30 seconds from queries. After
starting something, wait about 35 seconds before deciding data is missing.

## eBPF

The probes are compiled with `make bpf` (a container with clang and libbpf)
and tested by running the Linux binary in a rootful, privileged container:

    podman -c vigil-test-root run --rm --privileged --pid host \
      -v $PWD/bin:/vigil:ro docker.io/library/alpine /vigil/vigil-linux-arm64 events

`vigil events` prints every event as JSON; with `-config` it also prints the
rules that would fire.

Pitfall: when a probe seems to miss events, compare with the kernel's own
tracing (`/sys/kernel/tracing/events/...`) before suspecting the BPF code.
That is how the perf-tracepoint problem in [decisions.md](decisions.md) was
found: ftrace saw `cat`'s `openat`, the BPF program was never called.

## Kubernetes

A single-node k3s cluster runs as a privileged container in the rootful VM:

    podman -c vigil-test-root run -d --name k3s --privileged --cgroupns=host \
      --tmpfs /run --tmpfs /var/run docker.io/rancher/k3s:latest \
      server --disable traefik --disable metrics-server --disable servicelb --snapshotter native
    podman -c vigil-test-root exec -i k3s kubectl get nodes

Load the agent image into it and deploy:

    podman -c vigil-test-root build -t vigil:dev .
    podman -c vigil-test-root save localhost/vigil:dev | podman -c vigil-test-root exec -i k3s ctr images import -
    sed -e 's#image: vigil:dev .*#image: localhost/vigil:dev\n          imagePullPolicy: Never#' deploy/k8s-agent.yaml \
      | podman -c vigil-test-root exec -i k3s kubectl apply -f -

Pods reach a server on the Mac at its LAN address, so the server must
listen on 0.0.0.0. In zsh, `K="podman exec -i k3s kubectl"; $K get pods`
does not work (no word splitting); use a function.

## UI

Check UI changes in a browser against a running server with real data. Two
bugs that broke whole pages (stale panel data after reordering, one bad
panel taking the page down) were only visible that way.

The built-in browser pane refuses a server that uses its own certificate
authority (`tls.self_signed`). For UI checks against such a server, put a
plain-HTTP reverse proxy in front on 127.0.0.1 (a dozen lines of Go with
`httputil.NewSingleHostReverseProxy` and the authority's `ca.pem` as root)
and open that instead.

## TLS and enrolment

Test both with a real server and real agents, not only the unit test: the
live run found that an agent still on `http://` dropped its batches when the
server switched to TLS (Go answers such requests with a plain 400).

## Traces

A VictoriaTraces for local tests runs as `vigil-vt` in the test VM
(`podman -c vigil-test run -d --name vigil-vt -p 127.0.0.1:20428:10428
docker.io/victoriametrics/victoria-traces:v0.12.0 -storageDataPath=/vtraces`).
`vigil tracegen` sends demo traces to an agent's OTLP endpoint. A trace is
searchable a few seconds after it was sent, not at once.

## A real host: Docker and systemd

Podman's API stands in for Docker in the notes above. For the real thing
use a virtual machine: the original developer's is a Lima machine
configured by Ansible (kept outside the repository), Ubuntu 24.04 with
Docker from Docker's repository and the server and agent as systemd units.
Anything that reads the Docker API, journald or systemd is worth a run on
such a host; kernel 6.8 there is also older than what the eBPF probes had
been tried on.
