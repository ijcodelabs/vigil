package logs

import (
	"testing"

	"github.com/ijcodelabs/vigil/internal/dockerapi"
)

func TestContainerTemplate(t *testing.T) {
	patterns := map[string]string{"haproxy*": "haproxy", "web": "nginx", "web*": "generic", "*-b": "second", "postgis": "pg"}
	for _, c := range []struct{ name, image, want string }{
		{"haproxy-edge", "haproxy:3.0-alpine", "haproxy"},
		{"edge", "registry.example/library/haproxy:3.0@sha256:abc", "haproxy"}, // by image name
		{"web", "nginx:alpine", "nginx"},                                       // the exact name beats web*
		{"web-viewer", "shop-web-viewer", "generic"},
		{"webshop-b", "shop-api", "generic"}, // web* is longer than *-b
		{"postgres", "postgis/postgis:16-3.4", "pg"},
		{"redis", "redis:7-alpine", ""},
	} {
		if got := containerTemplate(patterns, c.name, c.image); got != c.want {
			t.Errorf("%s (%s): %q, want %q", c.name, c.image, got, c.want)
		}
	}
	if containerTemplate(nil, "x", "y") != "" {
		t.Error("no patterns should choose nothing")
	}
}

// Whose logs are collected: the container's label first, then the most
// specific pattern on its name or image, and everything by default.
func TestContainerLogsWanted(t *testing.T) {
	d := &Docker{Collect: map[string]string{"haproxy*": "false", "haproxy-edge": "true", "busybox": "false"}}
	for _, c := range []struct {
		name, image, label string
		want               bool
	}{
		{"web", "nginx:alpine", "", true},
		{"haproxy-internal", "haproxy:3.0", "", false},
		{"haproxy-edge", "haproxy:3.0", "", true},           // the exact name beats the pattern
		{"job-1", "docker.io/library/busybox:1", "", false}, // by image name
		{"web", "nginx:alpine", "false", false},
		{"haproxy-internal", "haproxy:3.0", "true", true}, // the label has the last word
	} {
		ct := dockerapi.Container{ID: "x", Names: []string{"/" + c.name}, Image: c.image, Labels: map[string]string{}}
		if c.label != "" {
			ct.Labels["vigil.logs"] = c.label
		}
		if got := d.wanted(ct); got != c.want {
			t.Errorf("%s (%s, label %q): collected %v, want %v", c.name, c.image, c.label, got, c.want)
		}
	}
	// "*": false turns the map into a list of the wanted ones.
	only := &Docker{Collect: map[string]string{"*": "false", "shop-*": "true"}}
	if only.wanted(dockerapi.Container{ID: "a", Names: []string{"/redis"}}) || !only.wanted(dockerapi.Container{ID: "b", Names: []string{"/shop-api"}}) {
		t.Error(`"*": false with "shop-*": true should collect only shop-*`)
	}
}
