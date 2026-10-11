// Package kube is the small part of the Kubernetes API the agent needs:
// the pods of its node, and the node's kubelet. It talks to the API server
// directly with the pod's service account.
package kube

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	saDir    = "/var/run/secrets/kubernetes.io/serviceaccount"
	cacheTTL = 30 * time.Second
)

// ErrNotInCluster means the agent is not running in a pod.
var ErrNotInCluster = errors.New("not running in a Kubernetes cluster (KUBERNETES_SERVICE_HOST unset)")

type Pod struct {
	Name, Namespace, UID string
	Annotations, Labels  map[string]string
	NodeName, Phase, IP  string
	Ports                []int // declared container ports, in order
	Containers           []Container
}

type Container struct {
	Name         string
	ID           string // runtime id without the "containerd://" prefix; empty until started
	Image        string
	Ready        bool
	RestartCount int
	State        string // running, waiting or terminated
	Reason       string // e.g. CrashLoopBackOff, OOMKilled
}

// Client lists the pods of one node, caching the answer briefly. It is
// shared by everything in the agent that needs pod information.
type Client struct {
	Node string // empty: all pods in the cluster

	// APIURL, HTTP and Token are set by InCluster; tests set them directly.
	APIURL string
	HTTP   *http.Client
	Token  func() (string, error)

	mu      sync.Mutex
	pods    []Pod
	fetched time.Time
}

// InCluster returns a client using the pod's service account.
func InCluster(node string) (*Client, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" {
		return nil, ErrNotInCluster
	}
	ca, err := os.ReadFile(saDir + "/ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &Client{
		Node:   node,
		APIURL: "https://" + net.JoinHostPort(host, port),
		Token:  ServiceAccountToken,
		HTTP: &http.Client{Timeout: 10 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}},
	}, nil
}

// ServiceAccountToken reads the pod's token; projected tokens rotate, so it
// is read every time.
func ServiceAccountToken() (string, error) {
	b, err := os.ReadFile(saDir + "/token")
	return strings.TrimSpace(string(b)), err
}

type podList struct {
	Items []struct {
		Metadata struct {
			Name        string            `json:"name"`
			Namespace   string            `json:"namespace"`
			UID         string            `json:"uid"`
			Annotations map[string]string `json:"annotations"`
			Labels      map[string]string `json:"labels"`
		} `json:"metadata"`
		Spec struct {
			NodeName   string `json:"nodeName"`
			Containers []struct {
				Ports []struct {
					ContainerPort int `json:"containerPort"`
				} `json:"ports"`
			} `json:"containers"`
		} `json:"spec"`
		Status struct {
			Phase             string `json:"phase"`
			PodIP             string `json:"podIP"`
			ContainerStatuses []struct {
				Name         string                    `json:"name"`
				ContainerID  string                    `json:"containerID"`
				Image        string                    `json:"image"`
				Ready        bool                      `json:"ready"`
				RestartCount int                       `json:"restartCount"`
				State        map[string]map[string]any `json:"state"`
			} `json:"containerStatuses"`
		} `json:"status"`
	} `json:"items"`
}

// Pods returns the node's pods. If the API cannot be reached, the last
// known list is returned along with the error.
func (c *Client) Pods(ctx context.Context) ([]Pod, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.fetched.IsZero() && time.Since(c.fetched) < cacheTTL {
		return c.pods, nil
	}
	return c.refresh(ctx)
}

func (c *Client) refresh(ctx context.Context) ([]Pod, error) {
	u := c.APIURL + "/api/v1/pods"
	if c.Node != "" {
		u += "?fieldSelector=" + url.QueryEscape("spec.nodeName="+c.Node)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return c.pods, err
	}
	if c.Token != nil {
		tok, err := c.Token()
		if err != nil {
			return c.pods, err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return c.pods, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return c.pods, fmt.Errorf("list pods: HTTP %d", resp.StatusCode)
	}
	var pl podList
	if err := json.NewDecoder(resp.Body).Decode(&pl); err != nil {
		return c.pods, err
	}
	pods := make([]Pod, 0, len(pl.Items))
	for _, it := range pl.Items {
		p := Pod{Name: it.Metadata.Name, Namespace: it.Metadata.Namespace, UID: it.Metadata.UID,
			Annotations: it.Metadata.Annotations, Labels: it.Metadata.Labels,
			NodeName: it.Spec.NodeName, Phase: it.Status.Phase, IP: it.Status.PodIP}
		for _, ct := range it.Spec.Containers {
			for _, port := range ct.Ports {
				p.Ports = append(p.Ports, port.ContainerPort)
			}
		}
		for _, st := range it.Status.ContainerStatuses {
			ct := Container{Name: st.Name, Image: st.Image, Ready: st.Ready, RestartCount: st.RestartCount}
			if i := strings.Index(st.ContainerID, "://"); i >= 0 {
				ct.ID = st.ContainerID[i+3:]
			}
			for state, detail := range st.State {
				ct.State = state
				ct.Reason, _ = detail["reason"].(string)
			}
			p.Containers = append(p.Containers, ct)
		}
		pods = append(pods, p)
	}
	c.pods, c.fetched = pods, time.Now()
	return pods, nil
}

// Pod finds a pod by namespace and name. A pod that is not in the cached
// list may have just started, so the list is fetched again, at most every
// two seconds.
func (c *Client) Pod(ctx context.Context, namespace, name string) (Pod, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	find := func() (Pod, bool) {
		for _, p := range c.pods {
			if p.Namespace == namespace && p.Name == name {
				return p, true
			}
		}
		return Pod{}, false
	}
	if p, ok := find(); ok || time.Since(c.fetched) < 2*time.Second {
		return p, ok
	}
	c.refresh(ctx)
	return find()
}
