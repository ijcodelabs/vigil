package logs

import "testing"

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
