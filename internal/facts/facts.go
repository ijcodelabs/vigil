// Package facts describes the host an agent runs on. The server matches
// profile selectors against these, next to the labels from the agent's file.
package facts

import (
	"context"
	"github.com/ijcodelabs/vigil/internal/gpu"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/shirou/gopsutil/v4/host"
	"github.com/shirou/gopsutil/v4/mem"
)

// Names are the fact keys. A label in an agent's file cannot use them.
var Names = []string{"agent", "os", "arch", "kernel", "distro", "distro_version", "hostname",
	"cpus", "memory_gb", "kubernetes", "kubernetes_role", "runtime", "ebpf", "gpu", "version"}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// Collect returns what can be found out; a fact that cannot is left out.
func Collect(ctx context.Context, agentID, version string) map[string]string {
	f := map[string]string{"agent": agentID, "version": version, "os": runtime.GOOS, "arch": runtime.GOARCH,
		"cpus": strconv.Itoa(runtime.NumCPU())}
	if h, err := os.Hostname(); err == nil {
		f["hostname"] = h
	}
	if k, err := host.KernelVersionWithContext(ctx); err == nil && k != "" {
		f["kernel"] = k
	}
	if platform, _, ver, err := host.PlatformInformationWithContext(ctx); err == nil && platform != "" {
		f["distro"], f["distro_version"] = platform, ver
	}
	if m, err := mem.VirtualMemoryWithContext(ctx); err == nil {
		f["memory_gb"] = strconv.FormatUint((m.Total+(1<<29))>>30, 10)
	}
	f["kubernetes"] = strconv.FormatBool(os.Getenv("KUBERNETES_SERVICE_HOST") != "" || os.Getenv("NODE_NAME") != "")
	// In a cluster an agent is either one of the per-node ones (the
	// DaemonSet sets NODE_NAME) or the one that watches the cluster as a whole.
	switch {
	case os.Getenv("NODE_NAME") != "":
		f["kubernetes_role"] = "node"
	case os.Getenv("KUBERNETES_SERVICE_HOST") != "":
		f["kubernetes_role"] = "cluster"
	}

	// The container runtime, by its socket. Inside a container the host's
	// /run is expected under HOST_ROOT.
	root := os.Getenv("HOST_ROOT")
	for _, rt := range [][2]string{{"docker", "/var/run/docker.sock"}, {"containerd", "/run/containerd/containerd.sock"},
		{"containerd", "/run/k3s/containerd/containerd.sock"},
		{"crio", "/run/crio/crio.sock"}, {"podman", "/run/podman/podman.sock"}} {
		if _, err := os.Stat(filepath.Join(root, rt[1])); err == nil {
			f["runtime"] = rt[0]
			break
		}
	}
	// Which kind of GPU the host has, if any: "nvidia", "amd" or "nvidia,amd".
	if vendors := gpu.New().Vendors(); len(vendors) > 0 {
		f["gpu"] = strings.Join(vendors, ",")
	}
	// Whether runtime events can work: Linux with BTF, kernel 5.8 or newer.
	f["ebpf"] = "false"
	if runtime.GOOS == "linux" {
		if _, err := os.Stat(filepath.Join(envOr("HOST_SYS", "/sys"), "kernel/btf/vmlinux")); err == nil && kernelAtLeast(f["kernel"], 5, 8) {
			f["ebpf"] = "true"
		}
	}
	return f
}

func kernelAtLeast(release string, major, minor int) bool {
	parts := strings.SplitN(release, ".", 3)
	if len(parts) < 2 {
		return false
	}
	ma, err1 := strconv.Atoi(parts[0])
	mi, err2 := strconv.Atoi(strings.TrimRightFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' }))
	return err1 == nil && err2 == nil && (ma > major || (ma == major && mi >= minor))
}
