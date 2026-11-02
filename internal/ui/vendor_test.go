package ui

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

// TestVendoredFiles: the vendored libraries are the published files listed
// in VENDOR.md. A change to one, on purpose or not, must update the list.
func TestVendoredFiles(t *testing.T) {
	want := map[string]string{
		"standalone.module.js": "72284e8e9079c87817145df1110f74e8a2aa040b2fc384922e18dfcb46fc1fd7", // htm@3.1.1
		"uPlot.esm.js":         "5dd9b3281aa64b461b42d9945f6adb2649d346502b12281a9ae0d46599a80eba", // uplot@1.6.32
		"uPlot.min.css":        "df630c6a8d6f8eeaff264b50f73ce5b114f646ffd9a0bb74f049b0a00135fa04", // uplot@1.6.32
	}
	entries, err := os.ReadDir("static/vendor")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(want) {
		t.Errorf("%d files in static/vendor, %d in VENDOR.md", len(entries), len(want))
	}
	for name, sum := range want {
		b, err := os.ReadFile(filepath.Join("static/vendor", name))
		if err != nil {
			t.Error(err)
			continue
		}
		got := sha256.Sum256(b)
		if hex.EncodeToString(got[:]) != sum {
			t.Errorf("%s changed: update VENDOR.md and this test after checking the new file", name)
		}
	}
}
