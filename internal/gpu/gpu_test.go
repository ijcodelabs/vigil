package gpu

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ijcodelabs/vigil/internal/model"
)

// Output in the form nvidia-smi prints it for a query with
// --format=csv,noheader,nounits: fields separated by ", ", memory in MiB,
// and "[N/A]" where a card does not report something.
const smiGPUs = `0, GPU-8f6b2c1a-1111-2222-3333-444455556666, NVIDIA A100-SXM4-80GB, 535.129.03, 87, 54, 61234, 81920, 67, 312.45, 400.00, [N/A], 3, 0
1, GPU-0a0b0c0d-aaaa-bbbb-cccc-ddddeeeeffff, NVIDIA A100-SXM4-80GB, 535.129.03, 0, 0, 4, 81920, 31, 54.10, 400.00, [N/A], 0, 1
2, GPU-99999999-0000-0000-0000-000000000000, NVIDIA GeForce RTX 3090, 535.129.03, 12, 3, 1024, 24576, 45, [N/A], [N/A], 30, [N/A], [N/A]
`

const smiApps = `GPU-8f6b2c1a-1111-2222-3333-444455556666, 4242, 40000, /opt/conda/bin/python3.11
GPU-8f6b2c1a-1111-2222-3333-444455556666, 4243, 21000, /opt/conda/bin/python3.11
GPU-99999999-0000-0000-0000-000000000000, 777, 1000, /usr/lib/xorg/Xorg
GPU-unknown, 1, 1, ghost
`

func find(t *testing.T, all []model.Sample, name string, want map[string]string) (float64, bool) {
	t.Helper()
	for _, s := range all {
		if s.Name != name {
			continue
		}
		ok := true
		for k, v := range want {
			ok = ok && s.Labels[k] == v
		}
		if ok {
			return s.Value, true
		}
	}
	return 0, false
}

func TestNvidia(t *testing.T) {
	proc := t.TempDir()
	// Two training processes in one container, as the same user id.
	id := strings.Repeat("ab12", 16)
	for _, pid := range []string{"4242", "4243"} {
		os.MkdirAll(filepath.Join(proc, pid), 0o755)
		os.WriteFile(filepath.Join(proc, pid, "status"), []byte("Name:\tpython3.11\nUid:\t65534\t65534\t65534\t65534\n"), 0o644)
		os.WriteFile(filepath.Join(proc, pid, "cgroup"), []byte("0::/system.slice/docker-"+id+".scope\n"), 0o644)
	}
	var asked [][]string
	c := &Collector{Processes: true, Proc: proc, Sys: t.TempDir(), Has: func(n string) bool { return n == "nvidia-smi" },
		ContainerNames: func() map[string]string { return map[string]string{id: "train-llm"} },
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			asked = append(asked, args)
			if strings.HasPrefix(args[0], "--query-gpu=") {
				return []byte(smiGPUs), nil
			}
			return []byte(smiApps), nil
		}}
	if v := c.Vendors(); len(v) != 1 || v[0] != "nvidia" {
		t.Fatalf("vendors: %v", v)
	}
	out, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]float64{
		"gpu_utilization_ratio": 0.87, "gpu_memory_utilization_ratio": 0.54, "gpu_memory_used_bytes": 61234 << 20,
		"gpu_memory_total_bytes": 81920 << 20, "gpu_temperature_celsius": 67, "gpu_power_watts": 312.45, "gpu_power_limit_watts": 400,
	} {
		if v, ok := find(t, out, name, map[string]string{"gpu": "0", "vendor": "nvidia"}); !ok || v < want*0.999 || v > want*1.001 {
			t.Errorf("%s of gpu 0: %v (found %v), want %v", name, v, ok, want)
		}
	}
	if v, ok := find(t, out, "gpu_info", map[string]string{"gpu": "0", "model": "NVIDIA A100-SXM4-80GB", "driver": "535.129.03"}); !ok || v != 1 {
		t.Error("gpu_info of gpu 0 missing")
	}
	if v, _ := find(t, out, "gpu_ecc_errors_total", map[string]string{"gpu": "0", "type": "corrected"}); v != 3 {
		t.Errorf("corrected errors of gpu 0: %v", v)
	}
	if v, _ := find(t, out, "gpu_ecc_errors_total", map[string]string{"gpu": "1", "type": "uncorrected"}); v != 1 {
		t.Errorf("uncorrected errors of gpu 1: %v", v)
	}
	// What a card does not report is left out, not sent as zero.
	for _, name := range []string{"gpu_fan_ratio"} {
		if _, ok := find(t, out, name, map[string]string{"gpu": "0"}); ok {
			t.Errorf("%s of gpu 0 should be absent", name)
		}
	}
	for _, name := range []string{"gpu_power_watts", "gpu_ecc_errors_total"} {
		if _, ok := find(t, out, name, map[string]string{"gpu": "2"}); ok {
			t.Errorf("%s of gpu 2 should be absent", name)
		}
	}
	if v, ok := find(t, out, "gpu_fan_ratio", map[string]string{"gpu": "2"}); !ok || v != 0.3 {
		t.Errorf("fan of gpu 2: %v %v", v, ok)
	}
	if v, _ := find(t, out, "gpu_count", map[string]string{"vendor": "nvidia"}); v != 3 {
		t.Errorf("gpu_count: %v", v)
	}
	if v, _ := find(t, out, "gpu_collector_up", map[string]string{"vendor": "nvidia"}); v != 1 {
		t.Errorf("gpu_collector_up: %v", v)
	}
	// Processes are added up by who holds the memory: program, user, container.
	if v, ok := find(t, out, "gpu_process_memory_bytes", map[string]string{"gpu": "0", "process": "python3.11", "container": "train-llm"}); !ok || v != 61000<<20 {
		t.Errorf("training processes on gpu 0: %v %v", v, ok)
	}
	if v, ok := find(t, out, "gpu_process_memory_bytes", map[string]string{"gpu": "2", "process": "Xorg"}); !ok || v != 1000<<20 {
		t.Errorf("Xorg on gpu 2: %v %v", v, ok)
	}
	if _, ok := find(t, out, "gpu_process_memory_bytes", map[string]string{"process": "ghost"}); ok {
		t.Error("a process on a GPU that is not there was reported")
	}

	// A card that dropped off the bus makes nvidia-smi fail: that is reported, not hidden.
	c.Run = func(context.Context, string, ...string) ([]byte, error) {
		return nil, errors.New("exit status 255: Unable to determine the device handle for GPU0000:3B:00.0: Unknown Error")
	}
	out, err = c.Collect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "device handle") {
		t.Errorf("failing nvidia-smi: %v", err)
	}
	if v, ok := find(t, out, "gpu_collector_up", map[string]string{"vendor": "nvidia"}); !ok || v != 0 {
		t.Errorf("gpu_collector_up while failing: %v %v", v, ok)
	}
	if _, ok := find(t, out, "gpu_count", nil); ok {
		t.Error("gpu_count should be absent while the tool fails, so that it does not read as zero cards")
	}
}

func TestAMD(t *testing.T) {
	sys := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(sys, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	write("class/drm/card0/device/vendor", "0x1002\n")
	write("class/drm/card0/device/gpu_busy_percent", "42\n")
	write("class/drm/card0/device/mem_info_vram_used", "2147483648\n")
	write("class/drm/card0/device/mem_info_vram_total", "17179869184\n")
	write("class/drm/card0/device/uevent", "DRIVER=amdgpu\nPCI_ID=1002:744C\n")
	write("class/drm/card0/device/hwmon/hwmon3/temp1_input", "61000\n")
	write("class/drm/card0/device/hwmon/hwmon3/power1_average", "187000000\n")
	write("class/drm/card0/device/hwmon/hwmon3/pwm1", "102\n")
	write("class/drm/card0/device/hwmon/hwmon3/pwm1_max", "255\n")
	write("class/drm/card0-DP-1/status", "connected\n") // a connector, not a card
	write("class/drm/card1/device/vendor", "0x8086\n")  // an Intel card: not ours
	write("class/drm/card1/device/gpu_busy_percent", "5\n")
	c := &Collector{Sys: sys, Proc: t.TempDir(), Has: func(string) bool { return false }}
	if v := c.Vendors(); len(v) != 1 || v[0] != "amd" {
		t.Fatalf("vendors: %v", v)
	}
	out, err := c.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]float64{"gpu_utilization_ratio": 0.42, "gpu_memory_used_bytes": 2147483648, "gpu_memory_total_bytes": 17179869184,
		"gpu_temperature_celsius": 61, "gpu_power_watts": 187, "gpu_fan_ratio": 0.4} {
		if v, ok := find(t, out, name, map[string]string{"gpu": "0", "vendor": "amd"}); !ok || v < want*0.999 || v > want*1.001 {
			t.Errorf("%s: %v (found %v), want %v", name, v, ok, want)
		}
	}
	if v, _ := find(t, out, "gpu_count", map[string]string{"vendor": "amd"}); v != 1 {
		t.Errorf("gpu_count: %v", v)
	}
	if _, ok := find(t, out, "gpu_info", map[string]string{"gpu": "1"}); ok {
		t.Error("the Intel card was reported")
	}
}
