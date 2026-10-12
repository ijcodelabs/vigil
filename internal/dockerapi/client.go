// Package dockerapi is the small part of the Docker Engine API the agent
// needs, spoken directly over the daemon's unix socket.
package dockerapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

type Client struct {
	base string
	hc   *http.Client
}

func New(socket string) *Client {
	return &Client{base: "http://docker", hc: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}}
}

// NewHTTP talks to a Docker API served over plain HTTP (tests).
func NewHTTP(base string) *Client { return &Client{base: base, hc: &http.Client{}} }

type Container struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Image  string            `json:"Image"`
	State  string            `json:"State"`
	Labels map[string]string `json:"Labels"`
}

// Name is the container name without Docker's leading slash.
func (c Container) Name() string {
	if len(c.Names) == 0 {
		return c.ID[:min(12, len(c.ID))]
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// ErrGone means the container asked about no longer exists. Containers
// that live for a second (health checks, one-off jobs) disappear between
// being listed and being looked at; that is not a fault.
var ErrGone = errors.New("container is gone")

func (c *Client) get(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		resp.Body.Close()
		err := fmt.Errorf("docker %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(msg)))
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %w", ErrGone, err)
		}
		return nil, err
	}
	return resp, nil
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	resp, err := c.get(ctx, path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return json.NewDecoder(resp.Body).Decode(out)
}

// List returns all containers, running or not.
func (c *Client) List(ctx context.Context) ([]Container, error) {
	var out []Container
	return out, c.getJSON(ctx, "/containers/json?all=1", &out)
}

// HasTTY reports whether the container's output is a raw TTY stream rather
// than Docker's multiplexed stdout/stderr framing.
func (c *Client) HasTTY(ctx context.Context, id string) (bool, error) {
	var v struct {
		Config struct{ Tty bool }
	}
	return v.Config.Tty, c.getJSON(ctx, "/containers/"+id+"/json", &v)
}

// RunState is what the daemon knows about whether a container runs.
type RunState struct {
	Running      bool
	ExitCode     int
	RestartCount int // restarts by the restart policy since the container was created
}

// RunState asks the daemon for a container's state. The list has the state
// too, but not the exit code or the restart count.
func (c *Client) RunState(ctx context.Context, id string) (RunState, error) {
	var v struct {
		State struct {
			Running  bool
			ExitCode int
		}
		RestartCount int
	}
	err := c.getJSON(ctx, "/containers/"+id+"/json", &v)
	return RunState{Running: v.State.Running, ExitCode: v.State.ExitCode, RestartCount: v.RestartCount}, err
}

// Logs follows a container's output with timestamps, starting after since
// (a unix time, optionally with a nanosecond fraction; "0" for everything).
func (c *Client) Logs(ctx context.Context, id, since string) (io.ReadCloser, error) {
	q := url.Values{"follow": {"1"}, "stdout": {"1"}, "stderr": {"1"}, "timestamps": {"1"}, "since": {since}}
	resp, err := c.get(ctx, "/containers/"+id+"/logs?"+q.Encode())
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

type Stats struct {
	CPU struct {
		Usage struct {
			Total uint64 `json:"total_usage"` // nanoseconds
		} `json:"cpu_usage"`
	} `json:"cpu_stats"`
	Memory struct {
		Usage uint64            `json:"usage"`
		Limit uint64            `json:"limit"`
		Stats map[string]uint64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes uint64 `json:"rx_bytes"`
		TxBytes uint64 `json:"tx_bytes"`
	} `json:"networks"`
}

// Stats takes a single resource-usage reading of a running container.
func (c *Client) Stats(ctx context.Context, id string) (Stats, error) {
	var s Stats
	return s, c.getJSON(ctx, "/containers/"+id+"/stats?stream=false&one-shot=true", &s)
}
