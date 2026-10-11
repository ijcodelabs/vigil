// Package hostmetrics collects CPU, memory, disk and network metrics of the
// machine the agent runs on. Inside a container, set HOST_PROC and HOST_SYS
// to the mounted host paths, and HOST_ROOT to where the host's / is mounted,
// so the numbers describe the host.
package hostmetrics

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/disk"
	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	"github.com/shirou/gopsutil/v4/net"

	"github.com/ijcodelabs/vigil/internal/model"
)

// realFS are filesystem types that hold data worth watching; everything else
// (proc, tmpfs, overlay, cgroup, ...) is kernel plumbing.
var realFS = map[string]bool{"ext2": true, "ext3": true, "ext4": true, "xfs": true, "btrfs": true, "zfs": true,
	"f2fs": true, "vfat": true, "exfat": true, "ntfs": true, "ntfs3": true, "fuseblk": true, "nfs": true, "nfs4": true,
	"cifs": true, "smb3": true, "cephfs": true, "glusterfs": true, "apfs": true, "hfs": true}

// Collect returns whatever it could read; one failing source does not hide
// the others. The last error, if any, is returned alongside.
func Collect(ctx context.Context) ([]model.Sample, error) {
	ts := time.Now().UnixMilli()
	var out []model.Sample
	var lastErr error
	add := func(name string, v float64, kv ...string) {
		l := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			l[kv[i]] = kv[i+1]
		}
		out = append(out, model.Sample{Name: name, Labels: l, Value: v, TS: ts})
	}

	if t, err := cpu.TimesWithContext(ctx, false); err != nil {
		lastErr = err
	} else if len(t) > 0 {
		for mode, v := range map[string]float64{
			"user": t[0].User, "system": t[0].System, "idle": t[0].Idle, "nice": t[0].Nice,
			"iowait": t[0].Iowait, "irq": t[0].Irq, "softirq": t[0].Softirq, "steal": t[0].Steal,
		} {
			add("host_cpu_seconds_total", v, "mode", mode)
		}
	}
	if n, err := cpu.CountsWithContext(ctx, true); err == nil {
		add("host_cpu_count", float64(n))
	}
	if l, err := load.AvgWithContext(ctx); err != nil {
		lastErr = err
	} else {
		add("host_load1", l.Load1)
		add("host_load5", l.Load5)
		add("host_load15", l.Load15)
	}
	if m, err := mem.VirtualMemoryWithContext(ctx); err != nil {
		lastErr = err
	} else {
		add("host_memory_total_bytes", float64(m.Total))
		add("host_memory_available_bytes", float64(m.Available))
		add("host_memory_used_bytes", float64(m.Used))
	}
	if s, err := mem.SwapMemoryWithContext(ctx); err == nil {
		add("host_swap_total_bytes", float64(s.Total))
		add("host_swap_used_bytes", float64(s.Used))
	}
	if parts, err := disk.PartitionsWithContext(ctx, true); err != nil {
		lastErr = err
	} else {
		// One entry per device: bind mounts and container mounts repeat the
		// same filesystem many times; the shortest mountpoint is the real one.
		byDevice := map[string]disk.PartitionStat{}
		for _, p := range parts {
			if !realFS[p.Fstype] {
				continue
			}
			if cur, ok := byDevice[p.Device]; !ok || len(p.Mountpoint) < len(cur.Mountpoint) {
				byDevice[p.Device] = p
			}
		}
		// In a container the host's filesystems are only reachable under
		// the directory its root is mounted at.
		root := os.Getenv("HOST_ROOT")
		for _, p := range byDevice {
			u, err := disk.UsageWithContext(ctx, filepath.Join(root, p.Mountpoint))
			if err != nil {
				continue
			}
			kv := []string{"mountpoint", p.Mountpoint, "device", p.Device, "fstype", p.Fstype}
			// A filesystem that went read-only is usually a failing disk.
			readonly := 0.0
			if slices.Contains(p.Opts, "ro") {
				readonly = 1
			}
			add("host_filesystem_readonly", readonly, kv...)
			add("host_filesystem_size_bytes", float64(u.Total), kv...)
			add("host_filesystem_free_bytes", float64(u.Free), kv...)
			add("host_filesystem_inodes_total", float64(u.InodesTotal), kv...)
			add("host_filesystem_inodes_free", float64(u.InodesFree), kv...)
		}
	}
	if io, err := disk.IOCountersWithContext(ctx); err == nil {
		for dev, c := range io {
			add("host_disk_read_bytes_total", float64(c.ReadBytes), "device", dev)
			add("host_disk_written_bytes_total", float64(c.WriteBytes), "device", dev)
			add("host_disk_reads_total", float64(c.ReadCount), "device", dev)
			add("host_disk_writes_total", float64(c.WriteCount), "device", dev)
		}
	}
	if nics, err := net.IOCountersWithContext(ctx, true); err != nil {
		lastErr = err
	} else {
		for _, n := range nics {
			add("host_network_receive_bytes_total", float64(n.BytesRecv), "device", n.Name)
			add("host_network_transmit_bytes_total", float64(n.BytesSent), "device", n.Name)
			add("host_network_receive_errors_total", float64(n.Errin), "device", n.Name)
			add("host_network_transmit_errors_total", float64(n.Errout), "device", n.Name)
			add("host_network_receive_dropped_total", float64(n.Dropin), "device", n.Name)
			add("host_network_transmit_dropped_total", float64(n.Dropout), "device", n.Name)
		}
	}
	if up, err := host.UptimeWithContext(ctx); err == nil {
		add("host_uptime_seconds", float64(up))
	}
	// A change in boot time is a reboot.
	if boot, err := host.BootTimeWithContext(ctx); err == nil {
		add("host_boot_time_seconds", float64(boot))
	}
	extra(add)
	return out, lastErr
}
