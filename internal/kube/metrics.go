package kube

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/ijcodelabs/vigil/internal/model"
	"github.com/ijcodelabs/vigil/internal/scrape"
)

// cadvisor metrics kept, and the names they are stored under. The names
// match what the Docker source reports, so the same dashboards and rules
// work for containers on plain hosts and in pods.
var containerMetrics = map[string]string{
	"container_cpu_usage_seconds_total":         "container_cpu_usage_seconds_total",
	"container_memory_working_set_bytes":        "container_memory_usage_bytes",
	"container_spec_memory_limit_bytes":         "container_memory_limit_bytes",
	"container_cpu_cfs_periods_total":           "container_cpu_cfs_periods_total",
	"container_cpu_cfs_throttled_periods_total": "container_cpu_cfs_throttled_periods_total",
	"container_oom_events_total":                "container_oom_events_total",
}

var podNetworkMetrics = map[string]bool{
	"container_network_receive_bytes_total":  true,
	"container_network_transmit_bytes_total": true,
}

// Kubelet reads per-container resource usage from the node's kubelet.
type Kubelet struct {
	URL   string // https://<node ip>:10250
	Token func() (string, error)
	HTTP  *http.Client
}

// NewKubelet addresses the kubelet of the node the pod runs on. The node's
// address comes from $NODE_IP (set from status.hostIP in the DaemonSet).
func NewKubelet() (*Kubelet, error) {
	ip := os.Getenv("NODE_IP")
	if ip == "" {
		return nil, fmt.Errorf("NODE_IP is not set; the DaemonSet sets it from status.hostIP")
	}
	return &Kubelet{
		URL:   "https://" + net.JoinHostPort(ip, "10250"),
		Token: ServiceAccountToken,
		// The kubelet's certificate is normally self-signed and not issued
		// for the node's IP; the bearer token is what authenticates us.
		HTTP: &http.Client{Timeout: 15 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
	}, nil
}

// Collect returns CPU, memory, throttling and OOM figures per container,
// and network traffic per pod, labelled namespace, pod and container.
func (k *Kubelet) Collect(ctx context.Context) ([]model.Sample, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.URL+"/metrics/cadvisor", nil)
	if err != nil {
		return nil, err
	}
	tok, err := k.Token()
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := k.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("kubelet: HTTP %d (does the service account have get on nodes/metrics?)", resp.StatusCode)
	}
	raw, err := scrape.Parse(io.LimitReader(resp.Body, 256<<20), time.Now().UnixMilli())
	if err != nil {
		return nil, err
	}
	var out []model.Sample
	for _, s := range raw {
		l := s.Labels
		if l["pod"] == "" {
			continue // host-level cgroups; the host metrics cover those
		}
		labels := map[string]string{"namespace": l["namespace"], "pod": l["pod"]}
		if name, ok := containerMetrics[s.Name]; ok {
			// An empty container name is the pod's total, "POD" its sandbox.
			if l["container"] == "" || l["container"] == "POD" {
				continue
			}
			labels["container"] = l["container"]
			if l["image"] != "" {
				labels["image"] = l["image"]
			}
			out = append(out, model.Sample{Name: name, Labels: labels, Value: s.Value, TS: s.TS})
		} else if podNetworkMetrics[s.Name] {
			labels["interface"] = l["interface"]
			out = append(out, model.Sample{Name: s.Name, Labels: labels, Value: s.Value, TS: s.TS})
		}
	}
	return out, nil
}

// PodState reports, from the API's pod list for this node, what phase each
// pod is in and how its containers are doing. The names follow
// kube-state-metrics, so rules written for it apply.
func (c *Client) PodState(ctx context.Context) ([]model.Sample, error) {
	pods, err := c.Pods(ctx)
	ts := time.Now().UnixMilli()
	var out []model.Sample
	add := func(name string, v float64, labels map[string]string) {
		out = append(out, model.Sample{Name: name, Labels: labels, Value: v, TS: ts})
	}
	for _, p := range pods {
		add("kube_pod_status_phase", 1, map[string]string{"namespace": p.Namespace, "pod": p.Name, "phase": p.Phase})
		for _, ct := range p.Containers {
			base := func() map[string]string {
				return map[string]string{"namespace": p.Namespace, "pod": p.Name, "container": ct.Name}
			}
			ready := 0.0
			if ct.Ready {
				ready = 1
			}
			add("kube_pod_container_status_ready", ready, base())
			add("kube_pod_container_status_restarts_total", float64(ct.RestartCount), base())
			if ct.State == "waiting" && ct.Reason != "" {
				l := base()
				l["reason"] = ct.Reason // CrashLoopBackOff, ImagePullBackOff, ...
				add("kube_pod_container_status_waiting_reason", 1, l)
			}
		}
	}
	return out, err
}
