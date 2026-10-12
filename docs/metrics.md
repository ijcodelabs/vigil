# Metric reference

Every series carries `agent` (who sent it) and the agent's own labels
(`env`, `role`, `cluster`, …). The tables list the labels each metric adds.
Names ending in `_total` are counters: graph them with `rate(...[5m])`.

In **Explore → Metrics**, **Show metrics** lists what actually has data.

## The host

| Metric | Meaning | Labels |
|---|---|---|
| `host_cpu_seconds_total` | CPU time spent | `mode`: user, system, idle, iowait, nice, irq, softirq, steal |
| `host_cpu_count` | number of CPUs | |
| `host_load1`, `host_load5`, `host_load15` | load average | |
| `host_memory_total_bytes`, `host_memory_available_bytes`, `host_memory_used_bytes` | memory | |
| `host_swap_total_bytes`, `host_swap_used_bytes` | swap | |
| `host_filesystem_size_bytes`, `host_filesystem_free_bytes` | disk space | `mountpoint`, `device`, `fstype` |
| `host_filesystem_inodes_total`, `host_filesystem_inodes_free` | inodes | same |
| `host_filesystem_readonly` | 1 if mounted read-only | same |
| `host_disk_read_bytes_total`, `host_disk_written_bytes_total` | disk throughput | `device` |
| `host_disk_reads_total`, `host_disk_writes_total` | disk operations | `device` |
| `host_network_receive_bytes_total`, `host_network_transmit_bytes_total` | network traffic | `device` |
| `host_network_receive_errors_total`, `host_network_transmit_errors_total` | network errors | `device` |
| `host_network_receive_dropped_total`, `host_network_transmit_dropped_total` | dropped packets | `device` |
| `host_pressure_ratio` | share of the last 10 s in which work waited for a resource (Linux) | `resource`: cpu, memory, io; `kind`: some, full |
| `host_file_descriptors`, `host_file_descriptors_limit` | open files, system-wide (Linux) | |
| `host_tcp_retransmits_total`, `host_tcp_segments_sent_total`, `host_tcp_connections` | TCP health (Linux) | |
| `host_oom_kills_total` | processes killed for lack of memory (Linux) | |
| `host_uptime_seconds`, `host_boot_time_seconds` | a change in boot time is a reboot | |

`host_filesystem_readonly` is 1 for filesystems that are read-only by
design too (some systems mount `/usr` that way); exclude those in a rule.

## Scraped targets

| Metric | Meaning | Labels |
|---|---|---|
| `up` | 1 if the last scrape worked | `job`, `instance` |
| `scrape_duration_seconds` | how long it took | `job`, `instance` |

Plus whatever the target itself exposes. If the target has its own `job` or
`instance` label, it is kept as `exported_job` / `exported_instance`.

## Containers

The same names for Docker containers and Kubernetes pods.

| Metric | Meaning | Labels |
|---|---|---|
| `container_cpu_usage_seconds_total` | CPU time used | `container`, `image`; on Kubernetes also `namespace`, `pod` |
| `container_memory_usage_bytes` | memory in use, without reclaimable cache | same |
| `container_memory_limit_bytes` | its limit; 0 or a huge number means none | same |
| `container_running` | 1 while it runs, 0 once it stopped; a removed container is no longer reported (Docker) | `container`, `image` |
| `container_exit_code` | the exit code of a container that is not running (Docker) | same |
| `container_restarts_total` | restarts by the restart policy since it was created (Docker) | same |
| `container_network_receive_bytes_total`, `container_network_transmit_bytes_total` | network traffic (per pod on Kubernetes) | `interface` |
| `container_cpu_cfs_periods_total`, `container_cpu_cfs_throttled_periods_total` | CPU limit enforcement (Kubernetes) | |
| `container_oom_events_total` | out-of-memory kills (Kubernetes) | |

## Kubernetes pods (node agents)

| Metric | Meaning | Labels |
|---|---|---|
| `kube_pod_status_phase` | 1 for the pod's phase | `namespace`, `pod`, `phase` |
| `kube_pod_container_status_ready` | 1 if ready | `namespace`, `pod`, `container` |
| `kube_pod_container_status_restarts_total` | restarts | same |
| `kube_pod_container_status_waiting_reason` | 1 while waiting | same, `reason` |

## Kubernetes cluster (cluster agent)

| Metric | Meaning | Labels |
|---|---|---|
| `kube_node_status_condition` | 1 if the condition holds | `node`, `condition`: Ready, MemoryPressure, DiskPressure, PIDPressure |
| `kube_node_spec_unschedulable` | 1 if cordoned | `node` |
| `kube_node_status_allocatable` | what pods may use | `node`, `resource`: cpu, memory, pods |
| `kube_node_info` | always 1; versions are in the labels | `node`, `kubelet_version`, `os_image`, `container_runtime_version`, `kernel_version` |
| `kube_deployment_spec_replicas`, `kube_deployment_status_replicas_available`, `_unavailable`, `_updated` | deployment replicas | `namespace`, `deployment` |
| `kube_deployment_spec_paused` | 1 if paused | same |
| `kube_statefulset_replicas`, `kube_statefulset_status_replicas_ready` | stateful sets | `namespace`, `statefulset` |
| `kube_daemonset_status_desired_number_scheduled`, `_number_ready`, `_number_unavailable` | daemon sets | `namespace`, `daemonset` |
| `kube_job_status_active`, `kube_job_status_succeeded`, `kube_job_status_failed` | jobs | `namespace`, `job_name` |
| `kube_cronjob_spec_suspend`, `kube_cronjob_status_last_schedule_time`, `kube_cronjob_status_last_successful_time` | cron jobs | `namespace`, `cronjob` |
| `kube_persistentvolumeclaim_status_phase` | 1 for the claim's phase | `namespace`, `persistentvolumeclaim`, `phase` |
| `kube_pod_status_unschedulable` | 1 for a pod no node will take | `namespace`, `pod` |

## Probes, processes, systemd

| Metric | Meaning | Labels |
|---|---|---|
| `probe_success` | 1 if it answered as expected | `probe`, `type` (http, tcp, tls, ping), `target` (what is checked; a URL without its user, password and query) |
| `probe_duration_seconds` | how long the answer took; for ping the average round trip | same |
| `probe_http_status_code` | the HTTP status, 0 if none | same |
| `probe_tls_cert_expiry_seconds` | time left on the certificate (https and tls); negative once it has expired, and reported also when the certificate is refused | same |
| `probe_packet_loss_ratio` | share of the three echo requests that got no answer (ping) | same |
| `process_group_count` | matching processes; 0 = not running | `group` |
| `process_group_cpu_seconds_total` | CPU time they used | `group` |
| `process_group_memory_rss_bytes`, `process_group_threads` | memory, threads | `group` |
| `systemd_units` | service units in each state | `state`: active, inactive, failed, … |
| `systemd_unit_state` | 1 for the current state of a named unit | `unit`, `state` |

## Applications (OpenTelemetry)

Whatever the application's SDK sends, with dots turned into underscores
(`http.server.duration` → `http_server_duration_bucket`, `_sum`, `_count`)
and `_total` added to counters. Labels: `service` and `job` from
`service.name`, `service_namespace`, `service_instance`,
`deployment_environment`, and the attributes of each data point.

## GPUs

See [GPUs and AI machines](gpu.md#what-is-collected): `gpu_info`,
`gpu_utilization_ratio`, `gpu_memory_used_bytes`, `gpu_memory_total_bytes`,
`gpu_temperature_celsius`, `gpu_power_watts`, `gpu_ecc_errors_total`,
`gpu_process_memory_bytes`, `gpu_count`, `gpu_collector_up`.

## Traces

Counted by the agent from every span it receives; see
[Services and traces](apm.md#metrics): `traces_spans_total`,
`traces_span_duration_seconds_*`, `traces_service_calls_total`,
`traces_service_call_duration_seconds_*`.

## vigil itself

| Metric | Meaning | Labels |
|---|---|---|
| `vigil_agent_up` | 1 while an agent reports (written by the server) | |
| `vigil_agent_last_seen_timestamp_seconds` | when it last reported | |
| `vigil_agent_limited_batches_total` | batches the server refused because the agent is over the server's limits; only present for agents this happened to | `signal` |
| `vigil_agent_info` | always 1 | `version`, `config`: the configuration it runs, or `local` |
| `vigil_agent_spool_bytes` | data waiting to be sent | `signal`: metrics, logs, events, captures |
| `vigil_agent_dropped_total` | what the agent dropped | `reason`: series_limit, log_rate_limit, spool_full; `unit` |
| `vigil_agent_series` | distinct series the agent is sending | |
| `vigil_agent_cpu_seconds_total` | processor time the agent itself has used; `rate()` of it is the share of one core | |
| `vigil_agent_memory_bytes` | memory the agent itself holds (resident size on Linux) | |
| `vigil_agent_open_files` | file descriptors the agent has open (Linux) | |
| `vigil_agent_goroutines` | goroutines in the agent; a number that only ever grows is a leak | |
| `vigil_agent_start_time_seconds` | when the agent started; `changes()` of it counts restarts | |
| `vigil_events_seen_total` | runtime events seen | `type` |
| `vigil_events_detections_total`, `vigil_events_suppressed_total`, `vigil_events_lost_total` | detections reported, held back by the rate limit, and events lost because the host was too busy | |
| `ALERTS` | 1 for each pending or firing alert | `alertname`, `alertstate`, and the alert's labels |
