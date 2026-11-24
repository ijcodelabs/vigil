package hostmetrics

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// write lays out files of a fake sysfs: path -> content.
func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(c+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCPUTemperatures(t *testing.T) {
	sys := t.TempDir()
	write(t, sys, map[string]string{
		// AMD with Tctl, Tdie and two CCDs: Tdie is the package value.
		"devices/pci0000:00/0000:00:18.3/x": "",
		"class/hwmon/hwmon3/name":           "k10temp",
		"class/hwmon/hwmon3/temp1_label":    "Tctl",
		"class/hwmon/hwmon3/temp1_input":    "71500",
		"class/hwmon/hwmon3/temp2_label":    "Tdie",
		"class/hwmon/hwmon3/temp2_input":    "61500",
		"class/hwmon/hwmon3/temp3_label":    "Tccd1",
		"class/hwmon/hwmon3/temp3_input":    "58000",
		"class/hwmon/hwmon3/temp4_label":    "Tccd2",
		"class/hwmon/hwmon3/temp4_input":    "55250",
		// Intel: the package sensor counts, per-core ones do not.
		"class/hwmon/hwmon5/name":        "coretemp",
		"class/hwmon/hwmon5/temp1_label": "Package id 1",
		"class/hwmon/hwmon5/temp1_input": "48000",
		"class/hwmon/hwmon5/temp2_label": "Core 0",
		"class/hwmon/hwmon5/temp2_input": "47000",
		// Something else entirely is ignored.
		"class/hwmon/hwmon0/name":        "nvme",
		"class/hwmon/hwmon0/temp1_input": "40000",
	})
	os.Symlink(filepath.Join(sys, "devices/pci0000:00/0000:00:18.3"), filepath.Join(sys, "class/hwmon/hwmon3/device"))

	var got []string
	cpuTemperatures(sys, func(name string, v float64, kv ...string) {
		got = append(got, strings.Join(append([]string{name}, kv...), " ")+" "+strconv.FormatFloat(v, 'g', -1, 64))
	})
	sort.Strings(got)
	want := []string{
		"host_cpu_temperature_celsius cpu 0 sensor ccd1 58",
		"host_cpu_temperature_celsius cpu 0 sensor ccd2 55.25",
		"host_cpu_temperature_celsius cpu 0 sensor package 61.5",
		"host_cpu_temperature_celsius cpu 1 sensor package 48",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestCPUTemperatureTctlOnlyAndPi(t *testing.T) {
	sys := t.TempDir()
	write(t, sys, map[string]string{
		"class/hwmon/hwmon1/name":        "k10temp",
		"class/hwmon/hwmon1/temp1_label": "Tctl",
		"class/hwmon/hwmon1/temp1_input": "52000",
		"class/hwmon/hwmon2/name":        "cpu_thermal",
		"class/hwmon/hwmon2/temp1_input": "45000",
	})
	n := 0
	cpuTemperatures(sys, func(name string, v float64, kv ...string) {
		n++
		if kv[3] != "package" || (v != 52 && v != 45) {
			t.Errorf("unexpected %v %v", kv, v)
		}
	})
	if n != 2 {
		t.Errorf("got %d samples, want 2", n)
	}
}
