// Package gpu reports graphics processors: how busy they are, their memory,
// temperature and power, memory errors, and which processes hold their
// memory. NVIDIA cards are read through nvidia-smi, which every machine
// with the driver has, so the agent stays a static binary; AMD cards are
// read from sysfs.
package gpu

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/model"
)

// queryFields are asked of nvidia-smi, in this order.
const queryFields = "index,uuid,name,driver_version,utilization.gpu,utilization.memory,memory.used,memory.total," +
	"temperature.gpu,power.draw,power.limit,fan.speed,ecc.errors.corrected.aggregate.total,ecc.errors.uncorrected.aggregate.total"

type Collector struct {
	// Processes adds which processes hold GPU memory.
	Processes bool
	// ContainerNames maps container ids to names, for the processes; optional.
	ContainerNames func() map[string]string

	// Run executes a program and returns its output; replaceable in tests.
	Run func(ctx context.Context, name string, args ...string) ([]byte, error)
	// Sys and Proc are where sysfs and procfs are (HOST_SYS, HOST_PROC in a container).
	Sys, Proc string
	// Has reports whether a program can be run.
	Has func(name string) bool
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func New() *Collector {
	return &Collector{Processes: true, Sys: envOr("HOST_SYS", "/sys"), Proc: envOr("HOST_PROC", "/proc"),
		Run: func(ctx context.Context, name string, args ...string) ([]byte, error) {
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, name, args...).Output()
			var ee *exec.ExitError
			if errors.As(err, &ee) && len(ee.Stderr) > 0 {
				err = fmt.Errorf("%w: %s", err, strings.TrimSpace(string(ee.Stderr)))
			}
			return out, err
		},
		Has: func(name string) bool { _, err := exec.LookPath(name); return err == nil },
	}
}

// Vendors says which kinds of GPU this host can report: "nvidia", "amd",
// both, or none.
func (c *Collector) Vendors() []string {
	var out []string
	if c.Has("nvidia-smi") {
		out = append(out, "nvidia")
	}
	if len(c.amdCards()) > 0 {
		out = append(out, "amd")
	}
	return out
}

// Collect returns, per GPU (label gpu is its index, vendor its maker):
//
//	gpu_info{model,uuid,driver}                 always 1
//	gpu_utilization_ratio                       share of the last sample period it was computing
//	gpu_memory_used_bytes, gpu_memory_total_bytes
//	gpu_memory_utilization_ratio                share of time its memory was being read or written (NVIDIA)
//	gpu_temperature_celsius, gpu_power_watts, gpu_power_limit_watts, gpu_fan_ratio
//	gpu_ecc_errors_total{type}                  memory errors, corrected and uncorrected (NVIDIA)
//	gpu_process_memory_bytes{process,user,container}   GPU memory held, by who holds it
//
// and for the host: gpu_count{vendor}, and gpu_collector_up{vendor}, which is
// 0 when the vendor's tool fails — as it does when a card has dropped off
// the bus.
func (c *Collector) Collect(ctx context.Context) ([]model.Sample, error) {
	ts := time.Now().UnixMilli()
	var out []model.Sample
	add := func(name string, v float64, kv ...string) {
		l := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] != "" {
				l[kv[i]] = kv[i+1]
			}
		}
		out = append(out, model.Sample{Name: name, Labels: l, Value: v, TS: ts})
	}
	var firstErr error
	if c.Has("nvidia-smi") {
		n, err := c.nvidia(ctx, add)
		up := 1.0
		if err != nil {
			up, firstErr = 0, fmt.Errorf("nvidia-smi: %w", err)
		} else {
			add("gpu_count", float64(n), "vendor", "nvidia")
		}
		add("gpu_collector_up", up, "vendor", "nvidia")
	}
	if cards := c.amdCards(); len(cards) > 0 {
		for _, card := range cards {
			c.amd(card, add)
		}
		add("gpu_count", float64(len(cards)), "vendor", "amd")
		add("gpu_collector_up", 1, "vendor", "amd")
	}
	return out, firstErr
}

// value reads one nvidia-smi field; ok is false for "[N/A]" and its kind.
func value(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "[") || strings.EqualFold(s, "N/A") {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	return v, err == nil
}

func (c *Collector) nvidia(ctx context.Context, add func(string, float64, ...string)) (int, error) {
	out, err := c.Run(ctx, "nvidia-smi", "--query-gpu="+queryFields, "--format=csv,noheader,nounits")
	if err != nil {
		return 0, err
	}
	byUUID := map[string]string{}
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ", ")
		if len(f) < 14 {
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		gpu := f[0]
		byUUID[f[1]] = gpu
		n++
		l := []string{"gpu", gpu, "vendor", "nvidia"}
		with := func(extra ...string) []string { return append(append([]string{}, l...), extra...) }
		add("gpu_info", 1, with("model", f[2], "uuid", f[1], "driver", f[3])...)
		set := func(name, raw string, scale float64, extra ...string) {
			if v, ok := value(raw); ok {
				add(name, v*scale, with(extra...)...)
			}
		}
		set("gpu_utilization_ratio", f[4], 0.01)
		set("gpu_memory_utilization_ratio", f[5], 0.01)
		set("gpu_memory_used_bytes", f[6], 1<<20)
		set("gpu_memory_total_bytes", f[7], 1<<20)
		set("gpu_temperature_celsius", f[8], 1)
		set("gpu_power_watts", f[9], 1)
		set("gpu_power_limit_watts", f[10], 1)
		set("gpu_fan_ratio", f[11], 0.01)
		set("gpu_ecc_errors_total", f[12], 1, "type", "corrected")
		set("gpu_ecc_errors_total", f[13], 1, "type", "uncorrected")
	}
	if n == 0 {
		return 0, errors.New("no GPU in its output: " + strings.TrimSpace(string(out[:min(len(out), 200)])))
	}
	if c.Processes {
		c.nvidiaProcesses(ctx, byUUID, add)
	}
	return n, nil
}

var containerIDRe = regexp.MustCompile(`[0-9a-f]{64}`)

// owner says who runs a process and in which container.
func (c *Collector) owner(pid string, names map[string]string) (usr, container string) {
	if b, err := os.ReadFile(filepath.Join(c.Proc, pid, "status")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if rest, ok := strings.CutPrefix(line, "Uid:"); ok {
				if f := strings.Fields(rest); len(f) > 0 {
					usr = f[0]
					if u, err := user.LookupId(f[0]); err == nil {
						usr = u.Username
					}
				}
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(c.Proc, pid, "cgroup")); err == nil {
		if id := containerIDRe.FindString(string(b)); id != "" {
			container = id[:12]
			if name := names[id]; name != "" {
				container = name
			} else if name := names[id[:12]]; name != "" {
				container = name
			}
		}
	}
	return usr, container
}

func (c *Collector) nvidiaProcesses(ctx context.Context, byUUID map[string]string, add func(string, float64, ...string)) {
	out, err := c.Run(ctx, "nvidia-smi", "--query-compute-apps=gpu_uuid,pid,used_memory,process_name", "--format=csv,noheader,nounits")
	if err != nil {
		return // older drivers lack this query; the per-GPU numbers stand on their own
	}
	var names map[string]string
	if c.ContainerNames != nil {
		names = c.ContainerNames()
	}
	type key struct{ gpu, process, user, container string }
	held := map[key]float64{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		f := strings.SplitN(sc.Text(), ", ", 4)
		if len(f) < 4 {
			continue
		}
		mem, ok := value(f[2])
		gpu, known := byUUID[strings.TrimSpace(f[0])]
		if !ok || !known {
			continue
		}
		usr, container := c.owner(strings.TrimSpace(f[1]), names)
		held[key{gpu, filepath.Base(strings.TrimSpace(f[3])), usr, container}] += mem * (1 << 20)
	}
	keys := make([]key, 0, len(held))
	for k := range held {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return fmt.Sprint(keys[i]) < fmt.Sprint(keys[j]) })
	for _, k := range keys {
		add("gpu_process_memory_bytes", held[k], "gpu", k.gpu, "vendor", "nvidia", "process", k.process, "user", k.user, "container", k.container)
	}
}

var cardRe = regexp.MustCompile(`^card[0-9]+$`)

// amdCards lists the DRM cards made by AMD that report how busy they are.
func (c *Collector) amdCards() []string {
	entries, err := os.ReadDir(filepath.Join(c.Sys, "class/drm"))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !cardRe.MatchString(e.Name()) {
			continue
		}
		dev := filepath.Join(c.Sys, "class/drm", e.Name(), "device")
		if vendor, _ := os.ReadFile(filepath.Join(dev, "vendor")); strings.TrimSpace(string(vendor)) != "0x1002" {
			continue
		}
		if _, err := os.Stat(filepath.Join(dev, "gpu_busy_percent")); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func readNumber(path string) (float64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(string(b)), 64)
	return v, err == nil
}

func (c *Collector) amd(card string, add func(string, float64, ...string)) {
	dev := filepath.Join(c.Sys, "class/drm", card, "device")
	l := []string{"gpu", strings.TrimPrefix(card, "card"), "vendor", "amd"}
	with := func(extra ...string) []string { return append(append([]string{}, l...), extra...) }
	model := ""
	if b, err := os.ReadFile(filepath.Join(dev, "uevent")); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if id, ok := strings.CutPrefix(line, "PCI_ID="); ok {
				model = "AMD " + id
			}
		}
	}
	add("gpu_info", 1, with("model", model)...)
	set := func(name, file string, scale float64) {
		if v, ok := readNumber(file); ok {
			add(name, v*scale, l...)
		}
	}
	set("gpu_utilization_ratio", filepath.Join(dev, "gpu_busy_percent"), 0.01)
	set("gpu_memory_used_bytes", filepath.Join(dev, "mem_info_vram_used"), 1)
	set("gpu_memory_total_bytes", filepath.Join(dev, "mem_info_vram_total"), 1)
	// Temperature, power and fan are under the card's hwmon directory.
	mons, _ := filepath.Glob(filepath.Join(dev, "hwmon", "hwmon*"))
	for _, m := range mons {
		set("gpu_temperature_celsius", filepath.Join(m, "temp1_input"), 0.001)
		if _, ok := readNumber(filepath.Join(m, "power1_average")); ok {
			set("gpu_power_watts", filepath.Join(m, "power1_average"), 1e-6)
		} else {
			set("gpu_power_watts", filepath.Join(m, "power1_input"), 1e-6)
		}
		if pwm, ok := readNumber(filepath.Join(m, "pwm1")); ok {
			if top, ok := readNumber(filepath.Join(m, "pwm1_max")); ok && top > 0 {
				add("gpu_fan_ratio", pwm/top, l...)
			}
		}
		break
	}
}
