# Kubernetes

A cluster gets two things:

| | Manifest | Runs as | Sees |
|---|---|---|---|
| **Node agents** | `deploy/k8s-agent.yaml` | DaemonSet, one per node | the node, its pods and containers, their logs |
| **Cluster agent** | `deploy/k8s-cluster-agent.yaml` | Deployment, exactly one | the cluster's objects and events |

## Install

1. In both manifests set the server address and replace `my-cluster` with
   this cluster's name. That name becomes the `cluster` label on everything,
   which is what keeps several clusters apart on one server.
2. Create the namespace and the secret. **Fleet → Enrolment → New enrolment
   token** shows the command under "Kubernetes":

   ```bash
   kubectl create namespace vigil
   kubectl -n vigil create secret generic vigil-agent \
     --from-literal=enroll-token=vge_… --from-literal=ca-pin=sha256:…
   ```

   Every node exchanges the enrolment token for a token of its own, under
   its node name, and keeps it on the node (`/var/lib/vigil`). Leave
   `ca-pin` out unless the server uses its own certificate authority.

   The cluster agent has no disk that outlives its pod, so it gets a token
   by hand: **Fleet → Add agent** with its `agent_id`, then

   ```bash
   kubectl -n vigil patch secret vigil-agent -p '{"stringData":{"cluster-token":"vgl_…"}}'
   ```

   (For a quick test, `--from-literal=token=<ingest token>` alone serves
   both, if the server has a shared `ingest_token`.)

3. Build the agent's image, push it to your registry and put its name in
   both manifests (they say `vigil-agent:dev`):

   ```bash
   docker build --target agent -t registry.example.com/vigil-agent:v1 .
   ```

   Then apply:

   ```bash
   kubectl apply -f deploy/k8s-agent.yaml -f deploy/k8s-cluster-agent.yaml
   ```

4. On the server, two profiles cover every cluster (they are in
   `configs/profiles/`):

   ```yaml
   # kubernetes.yaml
   select: ['kubernetes_role="node"']
   integrations: {kubernetes: {}}
   ```

   ```yaml
   # kubernetes-cluster.yaml
   select: ['kubernetes_role="cluster"']
   integrations: {kubernetes_cluster: {}}
   ```

Until a profile matches, each agent runs what its ConfigMap says, which is
the same thing.

> A profile that selects every agent also reaches the agents in clusters.
> If such a profile sets something that only makes sense on plain hosts,
> give it a selector, for example `kubernetes="false"`.

## What the node agents collect

- **Host metrics** of the node.
- **Metrics of pods that ask for it**, by annotation:

  ```yaml
  metadata:
    annotations:
      prometheus.io/scrape: "true"
      prometheus.io/port: "8080"      # default: the first declared container port
      prometheus.io/path: /metrics    # default
      prometheus.io/scheme: http      # default
  ```

  Labelled `namespace`, `pod`, `node`; job `kubernetes-pods`.
- **Every container's** CPU, memory, memory limit, CPU throttling and
  out-of-memory kills, and each pod's network traffic, from the node's
  kubelet. The metric names are the same as for Docker containers, so one
  dashboard serves both.
- **Pod state**: phase, container readiness, restarts and waiting reasons
  such as `CrashLoopBackOff` (`kube_pod_*`).
- **The logs of every pod**, labelled `namespace`, `pod`, `container` and
  `app` (from the pod's `app.kubernetes.io/name` or `app` label). A pod
  controls its own logs with annotations:

  ```yaml
  metadata:
    annotations:
      vigil.io/log-template: app_json   # a template from the agent's configuration
      vigil.io/logs: "false"            # or: do not collect this pod's logs
  ```

- **Runtime events** inside pods, if switched on; containers are named
  `namespace/pod/container`. See [Runtime events](security.md).

## What the cluster agent collects

- **Nodes**: ready and pressure conditions, cordoned, allocatable CPU,
  memory and pods, versions.
- **Deployments, stateful sets, daemon sets**: replicas wanted, available,
  unavailable.
- **Jobs and cron jobs**: active, succeeded, failed, last schedule.
- **Volume claims** by phase, and **pods no node will take**.
- **The cluster's events** as logs: `source:kubernetes-events`, with `type`
  (`Normal` or `Warning`), `reason`, and the `kind` and `name` of the object.

Metric names follow kube-state-metrics; see [Metric reference](metrics.md).

## Alerts and dashboards that come with it

The `kubernetes` integration: crash loops, frequent restarts, not ready,
stuck pending, out of memory, CPU throttling; dashboard **Kubernetes**.

The `kubernetes_cluster` integration: node not ready or under pressure,
replicas unavailable, stateful set or daemon set short, job failed, pod
unschedulable, volume claim pending; dashboard **Kubernetes cluster**.

## Useful queries

| Question | Where | Query |
|---|---|---|
| Why is my pod pending? | Logs | `source:kubernetes-events reason:FailedScheduling` |
| What is crashing? | Metrics | `kube_pod_container_status_waiting_reason{reason="CrashLoopBackOff"}` |
| Memory against limit | Metrics | `container_memory_usage_bytes / (container_memory_limit_bytes > 0 < 1e15)` |
| CPU per namespace | Metrics | `sum by (namespace) (rate(container_cpu_usage_seconds_total[5m]))` |
| Deployments missing replicas | Metrics | `kube_deployment_status_replicas_unavailable > 0` |
| A pod's logs | Logs | `namespace:shop pod:~"checkout-.*"` |

## Permissions

The node agent's service account may list pods and read `nodes/metrics`
(the kubelet). Its pod runs as root with the host's process namespace and
read-only mounts of `/proc`, `/sys`, `/etc`, `/` and `/var/log/pods`. It is
**privileged** only because runtime events need that; remove `privileged:
true` if you do not use them.

The cluster agent may list nodes, pods, events, volume claims, deployments,
stateful sets, daemon sets, jobs and cron jobs. It has no privileges and no
host access.

## Limits to know

- The cluster agent lists objects every 30 seconds rather than watching
  them. Very large clusters would want watches.
- Run exactly one cluster agent per cluster; two report everything twice.
- An object that was deleted lingers in counts for about five minutes.
- The kubelet's certificate is not verified (it is normally self-signed);
  the service account token is what authenticates the agent.
- Verified on a single-node k3s cluster with containerd.
- Not collected: autoscalers, ingresses, resource quotas, requests against
  limits per namespace.
