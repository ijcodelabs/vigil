// Package discovery turns configuration into concrete scrape targets.
package discovery

import (
	"context"
	"regexp"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/kube"
)

type Target struct {
	Job      string
	Instance string // host:port
	URL      string
	Labels   map[string]string
	Insecure bool // skip TLS verification

	// Zero means the collector's default.
	Interval time.Duration
	Timeout  time.Duration
	// At most one of BearerToken and Username is set.
	BearerToken        string
	BearerTokenFile    string // read on every scrape
	Username, Password string
	// Keep, if any, lists the only metric names to send; Drop removes names.
	Keep, Drop []*regexp.Regexp
}

type Discoverer interface {
	Name() string
	Targets(ctx context.Context) ([]Target, error)
}

// FromConfig builds the discoverers a scrape section asks for. pods is the
// agent's Kubernetes client; nil when it is not running in a cluster.
func FromConfig(c config.Scrape, pods *kube.Client, podsErr error) []Discoverer {
	var out []Discoverer
	if len(c.Static) > 0 {
		out = append(out, Static(c.Static))
	}
	if c.Kubernetes.Enabled {
		out = append(out, &Kubernetes{Client: pods, Insecure: c.Kubernetes.TLSInsecure,
			Interval: c.Kubernetes.Interval.Std(), initErr: podsErr})
	}
	return out
}
