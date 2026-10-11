package discovery

import (
	"context"
	"net"
	"strconv"
	"time"

	"github.com/ijcodelabs/vigil/internal/kube"
)

// Kubernetes finds the pods annotated prometheus.io/scrape: "true" among
// the pods the client lists (those of this node).
type Kubernetes struct {
	Client   *kube.Client
	Insecure bool
	// Interval overrides the collector's default for discovered pods.
	Interval time.Duration
	// initErr is why no client could be made; reported on every round.
	initErr error
}

func (*Kubernetes) Name() string { return "kubernetes" }

func (k *Kubernetes) Targets(ctx context.Context) ([]Target, error) {
	if k.Client == nil {
		return nil, k.initErr
	}
	pods, err := k.Client.Pods(ctx) // on failure: the last known pods, so scraping goes on
	var out []Target
	for _, p := range pods {
		a := p.Annotations
		if a["prometheus.io/scrape"] != "true" || p.Phase != "Running" || p.IP == "" {
			continue
		}
		port := a["prometheus.io/port"]
		if port == "" && len(p.Ports) > 0 { // fall back to the first declared container port
			port = strconv.Itoa(p.Ports[0])
		}
		if port == "" {
			continue
		}
		scheme, path := a["prometheus.io/scheme"], a["prometheus.io/path"]
		if scheme == "" {
			scheme = "http"
		}
		if path == "" {
			path = "/metrics"
		}
		addr := net.JoinHostPort(p.IP, port)
		out = append(out, Target{
			Job:      "kubernetes-pods",
			Instance: addr,
			URL:      scheme + "://" + addr + path,
			Labels:   map[string]string{"namespace": p.Namespace, "pod": p.Name, "node": p.NodeName},
			Insecure: k.Insecure,
			Interval: k.Interval,
		})
	}
	return out, err
}
