package kube

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

const podsJSON = `{"items":[
 {"metadata":{"name":"api-1","namespace":"prod","uid":"u1","annotations":{"vigil.io/log-template":"json"},"labels":{"app":"api"}},
  "spec":{"nodeName":"n1","containers":[{"ports":[{"containerPort":8080}]}]},
  "status":{"phase":"Running","podIP":"10.0.0.5","containerStatuses":[
    {"name":"api","containerID":"containerd://aaaabbbb","image":"acme/api:1","ready":true,"restartCount":2,"state":{"running":{"startedAt":"2026-01-01T00:00:00Z"}}},
    {"name":"sidecar","containerID":"containerd://ccccdddd","image":"acme/sc:1","ready":false,"restartCount":7,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}},
 {"metadata":{"name":"job-9","namespace":"batch","uid":"u2"},"spec":{"nodeName":"n1"},"status":{"phase":"Pending"}}
]}`

const cadvisor = `# TYPE container_cpu_usage_seconds_total counter
container_cpu_usage_seconds_total{container="api",cpu="total",id="/kubepods/x",image="acme/api:1",name="aaaabbbb",namespace="prod",pod="api-1"} 12.5 1700000000000
container_cpu_usage_seconds_total{container="",cpu="total",id="/kubepods/pod",image="",name="",namespace="prod",pod="api-1"} 13
container_cpu_usage_seconds_total{container="POD",cpu="total",id="/kubepods/sb",image="pause",name="x",namespace="prod",pod="api-1"} 0.1
container_cpu_usage_seconds_total{container="",cpu="total",id="/",image="",name="",namespace="",pod=""} 99999
container_memory_working_set_bytes{container="api",id="/kubepods/x",image="acme/api:1",name="aaaabbbb",namespace="prod",pod="api-1"} 5.24288e+07
container_spec_memory_limit_bytes{container="api",id="/kubepods/x",image="acme/api:1",name="aaaabbbb",namespace="prod",pod="api-1"} 2.68435456e+08
container_memory_rss{container="api",id="/kubepods/x",image="acme/api:1",name="aaaabbbb",namespace="prod",pod="api-1"} 1
container_network_receive_bytes_total{container="POD",id="/kubepods/sb",image="pause",interface="eth0",name="x",namespace="prod",pod="api-1"} 4096
`

func TestPodsAndState(t *testing.T) {
	calls := 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.RawQuery != "fieldSelector=spec.nodeName%3Dn1" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("query=%q auth=%q", r.URL.RawQuery, r.Header.Get("Authorization"))
		}
		io.WriteString(w, podsJSON)
	}))
	defer api.Close()
	c := &Client{Node: "n1", APIURL: api.URL, HTTP: api.Client(), Token: func() (string, error) { return "tok", nil }}
	ctx := context.Background()

	pods, err := c.Pods(ctx)
	if err != nil || len(pods) != 2 {
		t.Fatalf("%d pods, %v", len(pods), err)
	}
	p := pods[0]
	if p.Name != "api-1" || p.IP != "10.0.0.5" || p.Ports[0] != 8080 || p.Labels["app"] != "api" || len(p.Containers) != 2 ||
		p.Containers[0].ID != "aaaabbbb" || p.Containers[1].State != "waiting" || p.Containers[1].Reason != "CrashLoopBackOff" {
		t.Errorf("pod: %+v", p)
	}
	c.Pods(ctx)
	if got, ok := c.Pod(ctx, "prod", "api-1"); !ok || got.Annotations["vigil.io/log-template"] != "json" || calls != 1 {
		t.Errorf("cached lookup: ok=%v calls=%d", ok, calls)
	}

	samples, _ := c.PodState(ctx)
	got := map[string]float64{}
	for _, s := range samples {
		got[s.Name+"/"+s.Labels["pod"]+"/"+s.Labels["container"]+s.Labels["phase"]+s.Labels["reason"]] = s.Value
	}
	for k, want := range map[string]float64{
		"kube_pod_status_phase/api-1/Running": 1, "kube_pod_status_phase/job-9/Pending": 1,
		"kube_pod_container_status_ready/api-1/api": 1, "kube_pod_container_status_ready/api-1/sidecar": 0,
		"kube_pod_container_status_restarts_total/api-1/sidecar":                 7,
		"kube_pod_container_status_waiting_reason/api-1/sidecarCrashLoopBackOff": 1,
	} {
		if v, ok := got[k]; !ok || v != want {
			t.Errorf("%s = %v (present %v), want %v; all %v", k, v, ok, want, got)
		}
	}
}

func TestKubelet(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics/cadvisor" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		io.WriteString(w, cadvisor)
	}))
	defer srv.Close()
	k := &Kubelet{URL: srv.URL, HTTP: srv.Client(), Token: func() (string, error) { return "tok", nil }}
	samples, err := k.Collect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, s := range samples {
		if s.Labels["namespace"] != "prod" || s.Labels["pod"] != "api-1" || s.Labels["id"] != "" || s.Labels["name"] != "" {
			t.Errorf("labels of %s: %v", s.Name, s.Labels)
		}
		got[s.Name+"/"+s.Labels["container"]+s.Labels["interface"]] = s.Value
	}
	// Only the real container's figures, under the names the Docker source uses, and the pod's network.
	want := map[string]float64{"container_cpu_usage_seconds_total/api": 12.5, "container_memory_usage_bytes/api": 5.24288e+07,
		"container_memory_limit_bytes/api": 2.68435456e+08, "container_network_receive_bytes_total/eth0": 4096}
	if len(got) != len(want) {
		t.Errorf("got %v", got)
	}
	for name, v := range want {
		if got[name] != v {
			t.Errorf("%s = %v, want %v", name, got[name], v)
		}
	}
	k.Token = func() (string, error) { return "wrong", nil }
	if _, err := k.Collect(context.Background()); err == nil {
		t.Error("expected an error for a refused token")
	}
}
