package hostmetrics

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

func sysPath(parts ...string) string {
	root := os.Getenv("HOST_SYS")
	if root == "" {
		root = "/sys"
	}
	return filepath.Join(append([]string{root}, parts...)...)
}

var ccdLabel = regexp.MustCompile(`^Tccd(\d+)$`)

// cpuTemperatures reads the CPU temperature sensors the kernel exposes under
// /sys/class/hwmon as host_cpu_temperature_celsius{cpu, sensor}:
//
//   - sensor="package": the whole CPU. On AMD (k10temp, zenpower) Tdie when
//     there is one, else Tctl, which on some models carries a fan-control
//     offset; on Intel (coretemp) "Package id N"; on boards with only a
//     thermal zone for the SoC (cpu_thermal, e.g. a Raspberry Pi) that one.
//   - sensor="ccdN": each core complex die of an AMD CPU (Tccd1..).
//
// The same names are used by agents on other systems (a Windows monitor
// sends the same split), so one chart shows any host. Per-core sensors are
// left out: they add many series and the package value is what alerts need.
func cpuTemperatures(sys string, add func(name string, v float64, kv ...string)) {
	dirs, _ := filepath.Glob(filepath.Join(sys, "class", "hwmon", "hwmon*"))
	type chip struct{ dir, device string }
	var amd []chip
	for _, d := range dirs {
		name := readTrim(filepath.Join(d, "name"))
		switch name {
		case "k10temp", "zenpower":
			// One per socket. Order the sockets by their device address,
			// which unlike hwmonN does not change from boot to boot.
			dev, _ := filepath.EvalSymlinks(filepath.Join(d, "device"))
			amd = append(amd, chip{d, dev})
		case "coretemp":
			// "Package id N" names the socket itself.
			for label, file := range tempLabels(d) {
				if id, ok := strings.CutPrefix(label, "Package id "); ok {
					if v, ok := milli(file); ok {
						add("host_cpu_temperature_celsius", v, "cpu", id, "sensor", "package")
					}
				}
			}
		case "cpu_thermal":
			if v, ok := milli(filepath.Join(d, "temp1_input")); ok {
				add("host_cpu_temperature_celsius", v, "cpu", "0", "sensor", "package")
			}
		}
	}
	sort.Slice(amd, func(i, j int) bool { return amd[i].device < amd[j].device })
	for i, c := range amd {
		cpu := strconv.Itoa(i)
		labels := tempLabels(c.dir)
		pkg := labels["Tdie"]
		if pkg == "" {
			pkg = labels["Tctl"]
		}
		if pkg == "" && len(labels) == 0 { // old kernels: one unlabelled sensor
			pkg = filepath.Join(c.dir, "temp1_input")
		}
		if v, ok := milli(pkg); ok {
			add("host_cpu_temperature_celsius", v, "cpu", cpu, "sensor", "package")
		}
		for label, file := range labels {
			if m := ccdLabel.FindStringSubmatch(label); m != nil {
				if v, ok := milli(file); ok {
					add("host_cpu_temperature_celsius", v, "cpu", cpu, "sensor", "ccd"+m[1])
				}
			}
		}
	}
}

// tempLabels maps each tempN_label of a hwmon directory to its input file.
func tempLabels(dir string) map[string]string {
	out := map[string]string{}
	files, _ := filepath.Glob(filepath.Join(dir, "temp*_label"))
	for _, f := range files {
		if label := readTrim(f); label != "" {
			out[label] = strings.TrimSuffix(f, "_label") + "_input"
		}
	}
	return out
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// milli reads a sysfs temperature (millidegrees) as degrees.
func milli(path string) (float64, bool) {
	if path == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(readTrim(path), 64)
	if err != nil {
		return 0, false
	}
	return v / 1000, true
}
