package discovery

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ijcodelabs/vigil/internal/kube"
)

const pods = `{"items":[
 {"metadata":{"name":"api-1","namespace":"prod","annotations":{"prometheus.io/scrape":"true","prometheus.io/port":"9102","prometheus.io/path":"/m"}},
  "spec":{"nodeName":"n1"},"status":{"phase":"Running","podIP":"10.0.0.5"}},
 {"metadata":{"name":"web-1","namespace":"prod","annotations":{"prometheus.io/scrape":"true"}},
  "spec":{"nodeName":"n1","containers":[{"ports":[{"containerPort":8080}]}]},"status":{"phase":"Running","podIP":"10.0.0.6"}},
 {"metadata":{"name":"noscrape","namespace":"prod"},"spec":{"nodeName":"n1"},"status":{"phase":"Running","podIP":"10.0.0.7"}},
 {"metadata":{"name":"pending","namespace":"prod","annotations":{"prometheus.io/scrape":"true","prometheus.io/port":"1"}},
  "spec":{"nodeName":"n1"},"status":{"phase":"Pending"}}
]}`

func TestKubernetes(t *testing.T) {
	var gotQuery, gotAuth string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery, gotAuth = r.URL.RawQuery, r.Header.Get("Authorization")
		w.Write([]byte(pods))
	}))
	defer api.Close()

	k := &Kubernetes{Client: &kube.Client{Node: "n1", APIURL: api.URL, HTTP: api.Client(),
		Token: func() (string, error) { return "tok", nil }}}

	ts, err := k.Targets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotQuery != "fieldSelector=spec.nodeName%3Dn1" || gotAuth != "Bearer tok" {
		t.Errorf("query=%q auth=%q", gotQuery, gotAuth)
	}
	if len(ts) != 2 {
		t.Fatalf("got %d targets: %+v", len(ts), ts)
	}
	if ts[0].URL != "http://10.0.0.5:9102/m" || ts[0].Labels["pod"] != "api-1" {
		t.Errorf("target 0: %+v", ts[0])
	}
	if ts[1].URL != "http://10.0.0.6:8080/metrics" {
		t.Errorf("target 1: %+v", ts[1])
	}
}
