package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ijcodelabs/vigil/internal/model"
)

// list fetches every object of a kind, following the API's paging, and
// calls each with the raw items of a page.
func (c *Client) list(ctx context.Context, path string, query url.Values, each func(items json.RawMessage) error) error {
	if query == nil {
		query = url.Values{}
	}
	query.Set("limit", "500")
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.APIURL+path+"?"+query.Encode(), nil)
		if err != nil {
			return err
		}
		if c.Token != nil {
			tok, err := c.Token()
			if err != nil {
				return err
			}
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		var page struct {
			Metadata struct {
				Continue string `json:"continue"`
			} `json:"metadata"`
			Items json.RawMessage `json:"items"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("list %s: HTTP %d (is the cluster role bound?)", path, resp.StatusCode)
		}
		if err != nil {
			return err
		}
		if len(page.Items) > 0 {
			if err := each(page.Items); err != nil {
				return err
			}
		}
		if page.Metadata.Continue == "" {
			return nil
		}
		query.Set("continue", page.Metadata.Continue)
	}
}

type meta struct {
	Name              string    `json:"name"`
	Namespace         string    `json:"namespace"`
	UID               string    `json:"uid"`
	CreationTimestamp time.Time `json:"creationTimestamp"`
}

// quantity turns a Kubernetes quantity ("3900m", "16Gi", "110") into a
// number in base units: cores, bytes, or a count.
func quantity(s string) float64 {
	suffixes := []struct {
		s string
		f float64
	}{{"Ki", 1 << 10}, {"Mi", 1 << 20}, {"Gi", 1 << 30}, {"Ti", 1 << 40}, {"Pi", 1 << 50},
		{"m", 1e-3}, {"k", 1e3}, {"M", 1e6}, {"G", 1e9}, {"T", 1e12}}
	for _, suf := range suffixes {
		if num, ok := strings.CutSuffix(s, suf.s); ok {
			v, _ := strconv.ParseFloat(num, 64)
			return v * suf.f
		}
	}
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func b2f(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// ClusterState reports the state of the whole cluster's objects: nodes,
// deployments, stateful sets, daemon sets, jobs, cron jobs, volume claims,
// and pods that no node has taken. Names and labels follow
// kube-state-metrics. One agent per cluster runs this.
//
// If one kind cannot be listed the others are still reported; the last
// error is returned alongside.
func (c *Client) ClusterState(ctx context.Context) ([]model.Sample, error) {
	ts := time.Now().UnixMilli()
	var out []model.Sample
	var lastErr error
	add := func(name string, v float64, kv ...string) {
		l := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			l[kv[i]] = kv[i+1]
		}
		out = append(out, model.Sample{Name: name, Labels: l, Value: v, TS: ts})
	}
	each := func(path string, query url.Values, into any, fn func()) {
		err := c.list(ctx, path, query, func(items json.RawMessage) error {
			if err := json.Unmarshal(items, into); err != nil {
				return err
			}
			fn()
			return nil
		})
		if err != nil {
			lastErr = err
		}
	}

	var nodes []struct {
		Metadata meta `json:"metadata"`
		Spec     struct {
			Unschedulable bool `json:"unschedulable"`
		} `json:"spec"`
		Status struct {
			Allocatable map[string]string `json:"allocatable"`
			Conditions  []struct{ Type, Status string }
			NodeInfo    struct {
				KubeletVersion          string `json:"kubeletVersion"`
				OSImage                 string `json:"osImage"`
				ContainerRuntimeVersion string `json:"containerRuntimeVersion"`
				KernelVersion           string `json:"kernelVersion"`
			} `json:"nodeInfo"`
		} `json:"status"`
	}
	each("/api/v1/nodes", nil, &nodes, func() {
		for _, n := range nodes {
			node := n.Metadata.Name
			add("kube_node_info", 1, "node", node, "kubelet_version", n.Status.NodeInfo.KubeletVersion, "os_image", n.Status.NodeInfo.OSImage,
				"container_runtime_version", n.Status.NodeInfo.ContainerRuntimeVersion, "kernel_version", n.Status.NodeInfo.KernelVersion)
			add("kube_node_spec_unschedulable", b2f(n.Spec.Unschedulable), "node", node)
			for _, cond := range n.Status.Conditions {
				// One series per condition, 1 when it holds: Ready should be 1, the pressure ones 0.
				add("kube_node_status_condition", b2f(cond.Status == "True"), "node", node, "condition", cond.Type)
			}
			for _, res := range []string{"cpu", "memory", "pods"} {
				if q, ok := n.Status.Allocatable[res]; ok {
					add("kube_node_status_allocatable", quantity(q), "node", node, "resource", res)
				}
			}
		}
		nodes = nil
	})

	var deployments []struct {
		Metadata meta `json:"metadata"`
		Spec     struct {
			Replicas *int `json:"replicas"`
			Paused   bool `json:"paused"`
		} `json:"spec"`
		Status struct {
			AvailableReplicas   int `json:"availableReplicas"`
			UnavailableReplicas int `json:"unavailableReplicas"`
			UpdatedReplicas     int `json:"updatedReplicas"`
		} `json:"status"`
	}
	each("/apis/apps/v1/deployments", nil, &deployments, func() {
		for _, d := range deployments {
			kv := []string{"namespace", d.Metadata.Namespace, "deployment", d.Metadata.Name}
			want := 1
			if d.Spec.Replicas != nil {
				want = *d.Spec.Replicas
			}
			add("kube_deployment_spec_replicas", float64(want), kv...)
			add("kube_deployment_status_replicas_available", float64(d.Status.AvailableReplicas), kv...)
			add("kube_deployment_status_replicas_unavailable", float64(d.Status.UnavailableReplicas), kv...)
			add("kube_deployment_status_replicas_updated", float64(d.Status.UpdatedReplicas), kv...)
			add("kube_deployment_spec_paused", b2f(d.Spec.Paused), kv...)
		}
		deployments = nil
	})

	var statefulsets []struct {
		Metadata meta `json:"metadata"`
		Spec     struct {
			Replicas *int `json:"replicas"`
		} `json:"spec"`
		Status struct {
			ReadyReplicas int `json:"readyReplicas"`
		} `json:"status"`
	}
	each("/apis/apps/v1/statefulsets", nil, &statefulsets, func() {
		for _, s := range statefulsets {
			kv := []string{"namespace", s.Metadata.Namespace, "statefulset", s.Metadata.Name}
			want := 1
			if s.Spec.Replicas != nil {
				want = *s.Spec.Replicas
			}
			add("kube_statefulset_replicas", float64(want), kv...)
			add("kube_statefulset_status_replicas_ready", float64(s.Status.ReadyReplicas), kv...)
		}
		statefulsets = nil
	})

	var daemonsets []struct {
		Metadata meta `json:"metadata"`
		Status   struct {
			DesiredNumberScheduled int `json:"desiredNumberScheduled"`
			NumberReady            int `json:"numberReady"`
			NumberUnavailable      int `json:"numberUnavailable"`
		} `json:"status"`
	}
	each("/apis/apps/v1/daemonsets", nil, &daemonsets, func() {
		for _, d := range daemonsets {
			kv := []string{"namespace", d.Metadata.Namespace, "daemonset", d.Metadata.Name}
			add("kube_daemonset_status_desired_number_scheduled", float64(d.Status.DesiredNumberScheduled), kv...)
			add("kube_daemonset_status_number_ready", float64(d.Status.NumberReady), kv...)
			add("kube_daemonset_status_number_unavailable", float64(d.Status.NumberUnavailable), kv...)
		}
		daemonsets = nil
	})

	var jobs []struct {
		Metadata meta `json:"metadata"`
		Status   struct {
			Active    int `json:"active"`
			Succeeded int `json:"succeeded"`
			Failed    int `json:"failed"`
		} `json:"status"`
	}
	each("/apis/batch/v1/jobs", nil, &jobs, func() {
		for _, j := range jobs {
			kv := []string{"namespace", j.Metadata.Namespace, "job_name", j.Metadata.Name}
			add("kube_job_status_active", float64(j.Status.Active), kv...)
			add("kube_job_status_succeeded", float64(j.Status.Succeeded), kv...)
			add("kube_job_status_failed", float64(j.Status.Failed), kv...)
		}
		jobs = nil
	})

	var cronjobs []struct {
		Metadata meta `json:"metadata"`
		Spec     struct {
			Suspend bool `json:"suspend"`
		} `json:"spec"`
		Status struct {
			LastScheduleTime   *time.Time `json:"lastScheduleTime"`
			LastSuccessfulTime *time.Time `json:"lastSuccessfulTime"`
		} `json:"status"`
	}
	each("/apis/batch/v1/cronjobs", nil, &cronjobs, func() {
		for _, cj := range cronjobs {
			kv := []string{"namespace", cj.Metadata.Namespace, "cronjob", cj.Metadata.Name}
			add("kube_cronjob_spec_suspend", b2f(cj.Spec.Suspend), kv...)
			if t := cj.Status.LastScheduleTime; t != nil {
				add("kube_cronjob_status_last_schedule_time", float64(t.Unix()), kv...)
			}
			if t := cj.Status.LastSuccessfulTime; t != nil {
				add("kube_cronjob_status_last_successful_time", float64(t.Unix()), kv...)
			}
		}
		cronjobs = nil
	})

	var claims []struct {
		Metadata meta `json:"metadata"`
		Status   struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	each("/api/v1/persistentvolumeclaims", nil, &claims, func() {
		for _, pvc := range claims {
			add("kube_persistentvolumeclaim_status_phase", 1, "namespace", pvc.Metadata.Namespace,
				"persistentvolumeclaim", pvc.Metadata.Name, "phase", pvc.Status.Phase)
		}
		claims = nil
	})

	// Pods that no node has taken are invisible to the per-node agents.
	var unscheduled []struct {
		Metadata meta `json:"metadata"`
		Status   struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	each("/api/v1/pods", url.Values{"fieldSelector": {"spec.nodeName=,status.phase=Pending"}}, &unscheduled, func() {
		for _, p := range unscheduled {
			add("kube_pod_status_unschedulable", 1, "namespace", p.Metadata.Namespace, "pod", p.Metadata.Name)
			add("kube_pod_status_phase", 1, "namespace", p.Metadata.Namespace, "pod", p.Metadata.Name, "phase", p.Status.Phase)
		}
		unscheduled = nil
	})
	return out, lastErr
}

// Events returns the Kubernetes events last seen after `since`, as log
// entries, and the newest time among them. Warning events (failed
// scheduling, back-off, failed mounts, unhealthy probes) are usually the
// first place a problem shows.
func (c *Client) Events(ctx context.Context, since time.Time) ([]model.LogEntry, time.Time, error) {
	var events []struct {
		Metadata       meta      `json:"metadata"`
		Type           string    `json:"type"`
		Reason         string    `json:"reason"`
		Message        string    `json:"message"`
		Count          int       `json:"count"`
		LastTimestamp  time.Time `json:"lastTimestamp"`
		EventTime      time.Time `json:"eventTime"`
		InvolvedObject struct {
			Kind, Name, Namespace string
		} `json:"involvedObject"`
		Source struct {
			Component string `json:"component"`
			Host      string `json:"host"`
		} `json:"source"`
		ReportingComponent string `json:"reportingComponent"`
		// A recurring event keeps its first eventTime; the latest
		// occurrence and the running count are in series.
		Series *struct {
			Count            int       `json:"count"`
			LastObservedTime time.Time `json:"lastObservedTime"`
		} `json:"series"`
	}
	var out []model.LogEntry
	newest := since
	err := c.list(ctx, "/api/v1/events", nil, func(items json.RawMessage) error {
		if err := json.Unmarshal(items, &events); err != nil {
			return err
		}
		for _, ev := range events {
			at := ev.LastTimestamp
			if ev.Series != nil && ev.Series.LastObservedTime.After(at) {
				at = ev.Series.LastObservedTime
				ev.Count = ev.Series.Count
			}
			if at.IsZero() {
				at = ev.EventTime
			}
			if at.IsZero() {
				at = ev.Metadata.CreationTimestamp
			}
			if !at.After(since) || ev.Message == "" {
				continue
			}
			if at.After(newest) {
				newest = at
			}
			ns := ev.InvolvedObject.Namespace
			if ns == "" {
				ns = ev.Metadata.Namespace
			}
			e := model.LogEntry{Time: at.UnixNano(), Msg: ev.Message,
				Stream: map[string]string{"source": "kubernetes-events", "namespace": ns, "type": ev.Type},
				Fields: map[string]string{"reason": ev.Reason, "kind": ev.InvolvedObject.Kind, "name": ev.InvolvedObject.Name}}
			if ev.Type == "Warning" {
				e.Fields["level"] = "warning"
			} else {
				e.Fields["level"] = "info"
			}
			if ev.Count > 1 {
				e.Fields["count"] = strconv.Itoa(ev.Count)
			}
			if comp := ev.Source.Component; comp != "" {
				e.Fields["component"] = comp
			} else if ev.ReportingComponent != "" {
				e.Fields["component"] = ev.ReportingComponent
			}
			if ev.Source.Host != "" {
				e.Fields["node"] = ev.Source.Host
			}
			out = append(out, e)
		}
		events = nil
		return nil
	})
	return out, newest, err
}
