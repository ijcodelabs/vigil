# GPUs and AI machines

An agent reports the graphics processors of its host **by itself**: nothing
to switch on. It needs one of

- **NVIDIA**: `nvidia-smi` on the host (it comes with the driver);
- **AMD**: a card driven by `amdgpu` (read from `/sys/class/drm`).

The agent then has the fact `gpu` (`nvidia`, `amd`), so profiles can select
GPU hosts, and the **GPUs** integration adds alert rules and a dashboard:

```yaml
# a profile; configs/profiles/gpu.yaml is this
select: ['gpu=~".+"']
integrations:
  gpu: {}            # hot_celsius: "88", idle_hours: "6"
```

## What is collected

Per GPU (`gpu` is its index on the host, `vendor` its maker):

| Metric | Meaning |
|---|---|
| `gpu_info{model, uuid, driver}` | always 1; what the card is |
| `gpu_utilization_ratio` | share of the time it was computing (0 to 1) |
| `gpu_memory_used_bytes`, `gpu_memory_total_bytes` | its memory |
| `gpu_memory_utilization_ratio` | share of the time its memory was read or written (NVIDIA) |
| `gpu_temperature_celsius` | |
| `gpu_power_watts`, `gpu_power_limit_watts` | what it draws, and may draw |
| `gpu_fan_ratio` | fan speed, 0 to 1 (cards with a fan of their own) |
| `gpu_ecc_errors_total{type}` | memory errors since the driver was loaded, `corrected` and `uncorrected` (NVIDIA cards with ECC) |
| `gpu_process_memory_bytes{process, user, container}` | GPU memory held, by program, user and container (NVIDIA) |

Per host: `gpu_count{vendor}`, and `gpu_collector_up{vendor}`, which is 0
while the vendor's tool fails. What a card does not report is left out
rather than sent as zero.

Settings, all optional:

```yaml
gpu:
  enabled: true        # default: on where a GPU can be read; false switches it off
  processes: true      # who holds GPU memory
  interval: 15s        # at least 15s
```

## Alerts that come with the integration

| Alert | When |
|---|---|
| `GpuUnreadable` | the vendor's tool fails for five minutes. On NVIDIA this is what a card that "fell off the bus" looks like: `nvidia-smi` fails for all cards |
| `GpuMissing` | fewer GPUs than earlier the same day |
| `GpuMemoryErrors` | an uncorrectable memory error in the last hour |
| `GpuHot` | hotter than `hot_celsius` for ten minutes |
| `GpuIdle` | held more than 1 GB of memory for `idle_hours` while computing under 3%: a forgotten notebook or a stuck job on an expensive card |
| `GpuDriverError` | `NVRM: Xid` lines in the kernel log; needs journald logs (`logs.journald.enabled`) |

## Questions the data answers

```
# which GPUs did nothing today
sort(avg_over_time(gpu_utilization_ratio[24h]))

# who holds the memory of the busiest hosts
topk(10, sum by (agent, user, container) (gpu_process_memory_bytes))

# energy used by GPUs in a day, in kWh (samples are 15 s apart)
sum(sum_over_time(gpu_power_watts[1d]) * 15) / 3.6e6

# share of the fleet's GPU hours that were used, per day
avg(avg_over_time(gpu_utilization_ratio[1d]))
```

Idle GPUs are usually the largest avoidable cost of an AI fleet; see
[AI costs](finops.md) for putting a price on them.

## In containers and on Kubernetes

The agent needs `nvidia-smi` where it runs. As a host service that is
given. In a container, run the agent's container with the NVIDIA runtime
(`--gpus all`, or `runtimeClassName: nvidia` on the DaemonSet), which puts
`nvidia-smi` and the device files inside. For AMD cards nothing extra is
needed beyond the host's `/sys` (`HOST_SYS`).

Process owners are resolved from the host's `/proc` (`HOST_PROC`), and
container names from Docker where the agent has its socket.

## Not there yet

- Tested against the output format of `nvidia-smi` and against a made-up
  sysfs tree, not yet on real hardware.
- Per-process GPU *utilization* (only memory), MIG slices, NVLink and PCIe
  throughput, Intel GPUs.
- Kubernetes: which pod requested which GPU, and GPU capacity per node.
- Job schedulers (Slurm): which job runs on which GPU.
