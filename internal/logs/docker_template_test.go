package logs

import (
	"regexp"
	"testing"

	"github.com/ijcodelabs/vigil/internal/dockerapi"
	"github.com/ijcodelabs/vigil/internal/model"
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

// Drop rules see an entry as it would be stored and name whose lines they mean.
func TestDropRules(t *testing.T) {
	m := &Manager{drops: []dropRule{
		{where: map[string]string{"unit": "snapclient*"}, match: regexp.MustCompile(`Connection refused|not connected`)},
		{where: map[string]string{"container": "chatty"}}, // every line
		{where: map[string]string{"container": "web-*", "app": "nginx"}, match: regexp.MustCompile(`GET /healthz`)},
	}}
	entry := func(msg string, kv ...string) *model.LogEntry {
		e := &model.LogEntry{Msg: msg, Stream: map[string]string{}, Fields: map[string]string{}}
		for i := 0; i+1 < len(kv); i += 2 {
			e.Stream[kv[i]] = kv[i+1]
		}
		return e
	}
	for _, c := range []struct {
		e    *model.LogEntry
		want bool
	}{
		{entry("[Error] (Controller) Error: Connection refused", "unit", "snapclient.service"), true},
		{entry("[Notice] Connected to 10.0.0.3", "unit", "snapclient.service"), false},
		{entry("Connection refused", "unit", "sshd.service"), false}, // another unit
		{entry("anything at all", "container", "chatty"), true},
		{entry("GET /healthz 200", "container", "web-1", "app", "nginx"), true},
		{entry("GET /healthz 200", "container", "web-1"), false}, // both selectors must match
		{entry("GET /cart 200", "container", "web-1", "app", "nginx"), false},
	} {
		if got := m.dropped(c.e); got != c.want {
			t.Errorf("%q from %v: dropped %v, want %v", c.e.Msg, c.e.Stream, got, c.want)
		}
	}
}
