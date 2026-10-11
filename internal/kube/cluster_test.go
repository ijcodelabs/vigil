package kube

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClusterStateAndEvents(t *testing.T) {
	pages := map[string]int{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		pages[r.URL.Path]++
		switch r.URL.Path {
		case "/api/v1/nodes":
			io.WriteString(w, `{"items":[{"metadata":{"name":"n1"},"spec":{"unschedulable":true},"status":{
			  "allocatable":{"cpu":"3900m","memory":"16Gi","pods":"110"},
			  "conditions":[{"type":"Ready","status":"True"},{"type":"MemoryPressure","status":"False"}],
			  "nodeInfo":{"kubeletVersion":"v1.34.1","osImage":"Debian 13","containerRuntimeVersion":"containerd://2.1","kernelVersion":"6.12"}}}]}`)
		case "/apis/apps/v1/deployments":
			// Two pages, to check that paging is followed.
			if q.Get("continue") == "" {
				io.WriteString(w, `{"metadata":{"continue":"next"},"items":[{"metadata":{"name":"api","namespace":"prod"},"spec":{"replicas":3},"status":{"availableReplicas":1,"unavailableReplicas":2,"updatedReplicas":3}}]}`)
			} else {
				io.WriteString(w, `{"items":[{"metadata":{"name":"web","namespace":"prod"},"spec":{},"status":{"availableReplicas":1}}]}`)
			}
		case "/apis/apps/v1/statefulsets":
			io.WriteString(w, `{"items":[{"metadata":{"name":"db","namespace":"prod"},"spec":{"replicas":3},"status":{"readyReplicas":2}}]}`)
		case "/apis/apps/v1/daemonsets":
			http.Error(w, "forbidden", http.StatusForbidden) // one kind failing must not hide the rest
		case "/apis/batch/v1/jobs":
			io.WriteString(w, `{"items":[{"metadata":{"name":"backup-1","namespace":"ops"},"status":{"failed":2}}]}`)
		case "/apis/batch/v1/cronjobs":
			io.WriteString(w, `{"items":[{"metadata":{"name":"backup","namespace":"ops"},"spec":{"suspend":false},"status":{"lastScheduleTime":"2026-10-10T00:00:00Z"}}]}`)
		case "/api/v1/persistentvolumeclaims":
			io.WriteString(w, `{"items":[{"metadata":{"name":"data","namespace":"prod"},"status":{"phase":"Pending"}}]}`)
		case "/api/v1/pods":
			if q.Get("fieldSelector") != "spec.nodeName=,status.phase=Pending" {
				t.Errorf("pod selector: %q", q.Get("fieldSelector"))
			}
			io.WriteString(w, `{"items":[{"metadata":{"name":"big-1","namespace":"prod"},"status":{"phase":"Pending"}}]}`)
		case "/api/v1/events":
			io.WriteString(w, `{"items":[
			 {"metadata":{"namespace":"prod"},"type":"Warning","reason":"FailedScheduling","message":"0/1 nodes are available: 1 Insufficient memory.","count":4,
			  "lastTimestamp":"2026-10-10T12:00:10Z","involvedObject":{"kind":"Pod","name":"big-1","namespace":"prod"},"source":{"component":"default-scheduler"}},
			 {"metadata":{"namespace":"prod"},"type":"Normal","reason":"Pulled","message":"Container image already present","lastTimestamp":"2026-10-10T11:59:00Z",
			  "involvedObject":{"kind":"Pod","name":"api-1","namespace":"prod"},"source":{"component":"kubelet","host":"n1"}},
			 {"metadata":{"namespace":"prod","creationTimestamp":"2026-10-10T12:00:20Z"},"type":"Normal","reason":"ScalingReplicaSet","message":"Scaled up replica set api to 3",
			  "eventTime":null,"involvedObject":{"kind":"Deployment","name":"api","namespace":"prod"},"reportingComponent":"deployment-controller"},
			 {"metadata":{"namespace":"prod"},"type":"Warning","reason":"BackOff","message":"Back-off restarting failed container","eventTime":"2026-10-10T11:00:00.000000Z",
			  "series":{"count":9,"lastObservedTime":"2026-10-10T12:00:15.000000Z"},"involvedObject":{"kind":"Pod","name":"api-2","namespace":"prod"},"reportingComponent":"kubelet"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	c := &Client{APIURL: api.URL, HTTP: api.Client()}

	samples, err := c.ClusterState(context.Background())
	if err == nil || !strings.Contains(err.Error(), "daemonsets") {
		t.Errorf("the failing kind should be reported: %v", err)
	}
	got := map[string]float64{}
	for _, s := range samples {
		key := s.Name
		for _, l := range []string{"node", "condition", "resource", "deployment", "statefulset", "job_name", "cronjob", "persistentvolumeclaim", "pod", "phase"} {
			if v := s.Labels[l]; v != "" {
				key += "/" + v
			}
		}
		got[key] = s.Value
	}
	for key, want := range map[string]float64{
		"kube_node_status_condition/n1/Ready": 1, "kube_node_status_condition/n1/MemoryPressure": 0, "kube_node_spec_unschedulable/n1": 1,
		"kube_node_status_allocatable/n1/cpu": 3.9, "kube_node_status_allocatable/n1/memory": 16 << 30, "kube_node_status_allocatable/n1/pods": 110,
		"kube_node_info/n1":                 1,
		"kube_deployment_spec_replicas/api": 3, "kube_deployment_status_replicas_unavailable/api": 2,
		"kube_deployment_spec_replicas/web": 1, "kube_deployment_status_replicas_available/web": 1, // replicas unset means one
		"kube_statefulset_replicas/db": 3, "kube_statefulset_status_replicas_ready/db": 2,
		"kube_job_status_failed/backup-1": 2, "kube_cronjob_status_last_schedule_time/backup": 1791590400,
		"kube_persistentvolumeclaim_status_phase/data/Pending": 1,
		"kube_pod_status_unschedulable/big-1":                  1, "kube_pod_status_phase/big-1/Pending": 1,
	} {
		if v, ok := got[key]; !ok || v != want {
			t.Errorf("%s = %v (present %v), want %v", key, v, ok, want)
		}
	}
	if pages["/apis/apps/v1/deployments"] != 2 {
		t.Errorf("deployment pages fetched: %d", pages["/apis/apps/v1/deployments"])
	}

	since := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	entries, newest, err := c.Events(context.Background(), since)
	// A recurring event is reported by its latest occurrence, with the count so far.
	if n := len(entries); n == 3 && (entries[2].Fields["reason"] != "BackOff" || entries[2].Fields["count"] != "9") {
		t.Errorf("recurring event: %+v", entries[2])
	}
	if err != nil || len(entries) != 3 || !newest.Equal(since.Add(20*time.Second)) {
		t.Fatalf("%d events, newest %v, err %v", len(entries), newest, err)
	}
	e := entries[0]
	if e.Msg != "0/1 nodes are available: 1 Insufficient memory." || e.Stream["type"] != "Warning" || e.Stream["namespace"] != "prod" ||
		e.Stream["source"] != "kubernetes-events" || e.Fields["reason"] != "FailedScheduling" || e.Fields["kind"] != "Pod" ||
		e.Fields["name"] != "big-1" || e.Fields["count"] != "4" || e.Fields["level"] != "warning" || e.Fields["component"] != "default-scheduler" {
		t.Errorf("event: %+v", e)
	}
	if entries[1].Fields["component"] != "deployment-controller" || entries[1].Fields["level"] != "info" {
		t.Errorf("second event: %+v", entries[1])
	}
	// Asked again from the newest time, nothing is repeated.
	if again, _, _ := c.Events(context.Background(), newest); len(again) != 0 {
		t.Errorf("%d events repeated", len(again))
	}
}

func TestQuantity(t *testing.T) {
	for in, want := range map[string]float64{"3900m": 3.9, "4": 4, "16Gi": 16 << 30, "512Mi": 512 << 20, "110": 110, "1k": 1000, "": 0} {
		if got := quantity(in); got != want {
			t.Errorf("quantity(%q) = %v, want %v", in, got, want)
		}
	}
}
