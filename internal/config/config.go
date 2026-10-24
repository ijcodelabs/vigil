// Package config loads the agent and server YAML files.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration lets YAML use "15s" style values.
type Duration time.Duration

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("line %d: %w", n.Line, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) Std() time.Duration { return time.Duration(d) }

type Scrape struct {
	Static     []StaticJob `yaml:"static"`
	Kubernetes Kubernetes  `yaml:"kubernetes"`
	Kubelet    Kubelet     `yaml:"kubelet"`
}

// Kubelet collects, on a Kubernetes node, CPU, memory, throttling and
// network use of every container from the node's kubelet, and the phase,
// readiness and restarts of the node's pods from the API.
type Kubelet struct {
	Enabled  bool     `yaml:"enabled"`
	Interval Duration `yaml:"interval"`
}

type BasicAuth struct {
	Username string `yaml:"username"`
	Password string `yaml:"password"`
}

type StaticJob struct {
	Job         string            `yaml:"job"`
	Targets     []string          `yaml:"targets"`
	Scheme      string            `yaml:"scheme"`
	Path        string            `yaml:"path"`
	Labels      map[string]string `yaml:"labels"`
	TLSInsecure bool              `yaml:"tls_insecure_skip_verify"`
	// Interval and Timeout override the agent-wide ones for this job.
	Interval    Duration `yaml:"interval"`
	Timeout     Duration `yaml:"timeout"`
	BearerToken string   `yaml:"bearer_token"`
	// BearerTokenFile is a file on the agent's host holding the token, read
	// on every scrape: the token never passes through the server or a
	// profile, and a new one needs no restart.
	BearerTokenFile string     `yaml:"bearer_token_file"`
	BasicAuth       *BasicAuth `yaml:"basic_auth"`
	// Keep and Drop are regular expressions on metric names. With Keep set,
	// only matching metrics are sent; Drop removes matching ones.
	Keep []string `yaml:"keep"`
	Drop []string `yaml:"drop"`
	// Enabled false switches the job off; a profile uses it to turn off a
	// job that a broader profile defines.
	Enabled *bool `yaml:"enabled"`
}

// Kubernetes discovers pods annotated with prometheus.io/scrape: "true".
type Kubernetes struct {
	Enabled bool `yaml:"enabled"`
	// Node limits discovery to pods on one node. Defaults to $NODE_NAME;
	// empty means every pod in the cluster.
	Node        string   `yaml:"node"`
	TLSInsecure bool     `yaml:"tls_insecure_skip_verify"`
	Interval    Duration `yaml:"interval"`
}

// LogTemplate describes how to turn raw lines of one kind of log into
// entries: what to join, parse, extract, keep and hide.
type LogTemplate struct {
	Multiline struct {
		// Start matches the first line of an entry; other lines are appended
		// to the entry before them (stack traces).
		Start string `yaml:"start"`
	} `yaml:"multiline"`
	Parse        string `yaml:"parse"`   // "", json, logfmt or regex
	Pattern      string `yaml:"pattern"` // for regex: named groups become fields
	MessageField string `yaml:"message_field"`
	Timestamp    struct {
		Field  string `yaml:"field"`
		Layout string `yaml:"layout"` // rfc3339, unix, unix_ms or a Go layout
	} `yaml:"timestamp"`
	Labels []string `yaml:"labels"` // fields promoted to stream labels
	// Add puts fixed stream labels on every line the template handles, for
	// example app: haproxy, so that rules and dashboards can select them
	// wherever they came from.
	Add  map[string]string `yaml:"add"`
	Keep []string          `yaml:"keep"` // if set, only lines matching one of these
	Drop []string          `yaml:"drop"` // lines matching any of these are discarded
	Mask []struct {
		Pattern string `yaml:"pattern"`
		Replace string `yaml:"replace"`
	} `yaml:"mask"`
}

type LogFiles struct {
	// Name identifies the source so that a profile can change or switch off
	// one that a broader profile defines. Optional in a local file.
	Name          string            `yaml:"name"`
	Enabled       *bool             `yaml:"enabled"`
	Paths         []string          `yaml:"paths"`   // globs
	Exclude       []string          `yaml:"exclude"` // globs on the file name
	Template      string            `yaml:"template"`
	Labels        map[string]string `yaml:"labels"`
	FromBeginning bool              `yaml:"from_beginning"`
}

// LogDrop keeps lines from being sent: those of the sources it names whose
// message matches. The selectors (container, unit, pod, app) may use * and ?
// and must all match; at least one is needed. Match is a regular
// expression on the message as it would be stored; empty means every line.
// It is applied last, after the source's template, so it sees what a
// search in the server would see.
type LogDrop struct {
	Name      string `yaml:"name"`
	Container string `yaml:"container"`
	Unit      string `yaml:"unit"`
	Pod       string `yaml:"pod"`
	App       string `yaml:"app"`
	Match     string `yaml:"match"`
	Enabled   *bool  `yaml:"enabled"`
}

type Logs struct {
	Templates map[string]LogTemplate `yaml:"templates"`
	Drop      []LogDrop              `yaml:"drop"`
	Files     []LogFiles             `yaml:"files"`
	Journald  struct {
		Enabled  bool     `yaml:"enabled"`
		Units    []string `yaml:"units"`
		Template string   `yaml:"template"`
		// UnitTemplates chooses a template by unit: the key is matched
		// against the unit's name and the entry's syslog identifier and may
		// use * and ?. Lines are joined (multiline) per unit.
		UnitTemplates map[string]string `yaml:"unit_templates"`
		Labels        map[string]string `yaml:"labels"`
	} `yaml:"journald"`
	// Kubernetes tails the node's pod log files (CRI format). A pod picks
	// its template with the annotation vigil.io/log-template: <name> and
	// opts out with vigil.io/logs: "false".
	Kubernetes struct {
		Enabled bool   `yaml:"enabled"`
		Path    string `yaml:"path"` // default /var/log/pods
		// ContainerTemplates maps a container name to a template, for pods
		// without the annotation.
		ContainerTemplates map[string]string `yaml:"container_templates"`
		Labels             map[string]string `yaml:"labels"`
	} `yaml:"kubernetes"`
}

// Docker covers containers of the local Docker daemon: their logs and their
// CPU/memory/network metrics. A container opts out with the label
// vigil.logs=false and picks a template with vigil.logs.template=<name>.
type Docker struct {
	Enabled  bool   `yaml:"enabled"`
	Socket   string `yaml:"socket"`
	Logs     *bool  `yaml:"logs"`
	Metrics  *bool  `yaml:"metrics"`
	Template string `yaml:"template"` // default for containers without the label
	// ContainerTemplates chooses a template by container: the key is matched
	// against the container's name and its image's name (without registry
	// and tag) and may use * and ?. For containers you cannot label.
	ContainerTemplates map[string]string `yaml:"container_templates"`
	// ContainerLogs says whose logs are collected, by the same kind of key:
	// false for a container whose logs are not wanted, true to collect one
	// that a wider pattern switched off ("*": false makes it a list of the
	// wanted ones). The most specific pattern wins. A map, so that profiles
	// add to each other's entries.
	ContainerLogs map[string]bool   `yaml:"container_logs"`
	Labels        map[string]string `yaml:"labels"`
}

// Security turns on runtime events from the kernel (process starts, network
// connections, opens of watched files, privilege changes) and the rules
// evaluated on them, on the host itself. Linux 5.8 or newer with BTF.
type Security struct {
	Enabled bool `yaml:"enabled"`
	// WatchPaths are absolute path prefixes whose opens are reported.
	WatchPaths []string `yaml:"watch_paths"`
	// Buffer is how much recent activity is kept in memory for captures.
	Buffer Duration `yaml:"buffer"`
	// A capture holds the activity from CaptureBefore the triggering event
	// until CaptureAfter it.
	CaptureBefore   Duration       `yaml:"capture_before"`
	CaptureAfter    Duration       `yaml:"capture_after"`
	CaptureCooldown Duration       `yaml:"capture_cooldown"` // per rule
	Rules           []SecurityRule `yaml:"rules"`
}

type SecurityRule struct {
	Name     string `yaml:"name"`
	When     string `yaml:"when"`     // CEL expression over the event's fields
	Severity string `yaml:"severity"` // info, warning (default) or critical
	Summary  string `yaml:"summary"`  // text with {field} placeholders
	Capture  bool   `yaml:"capture"`  // also save a capture when this fires
	Enabled  *bool  `yaml:"enabled"`  // false switches an inherited rule off
	// MaxPerMinute caps how many detections this rule may report. Default 10.
	MaxPerMinute int `yaml:"max_per_minute"`
}

// Probe checks from the agent's host that something answers: an HTTP(S)
// address, a TCP port, a TLS service, or a host by ICMP echo ("ping"). For
// HTTPS and TLS it also reports how long the certificate is still valid.
type Probe struct {
	Name         string            `yaml:"name"`
	HTTP         string            `yaml:"http"` // a URL
	TCP          string            `yaml:"tcp"`  // host:port
	TLS          string            `yaml:"tls"`  // host:port of anything that speaks TLS at once (not STARTTLS)
	Ping         string            `yaml:"ping"` // a host name or address
	Interval     Duration          `yaml:"interval"`
	Timeout      Duration          `yaml:"timeout"`
	ExpectStatus []int             `yaml:"expect_status"` // default: any 2xx
	ExpectBody   string            `yaml:"expect_body"`   // HTTP: a regular expression the answer must contain
	TLSInsecure  bool              `yaml:"tls_insecure_skip_verify"`
	Labels       map[string]string `yaml:"labels"`
	Enabled      *bool             `yaml:"enabled"`
}

// Kind and Target say what a probe checks, in the words of its config.
func (p Probe) Kind() string {
	switch {
	case p.HTTP != "":
		return "http"
	case p.TCP != "":
		return "tcp"
	case p.TLS != "":
		return "tls"
	default:
		return "ping"
	}
}

func (p Probe) Target() string {
	switch {
	case p.HTTP != "":
		return p.HTTP
	case p.TCP != "":
		return p.TCP
	case p.TLS != "":
		return p.TLS
	default:
		return p.Ping
	}
}

// ProcessGroup reports CPU, memory and count of the processes it matches.
type ProcessGroup struct {
	Name    string `yaml:"name"`
	Match   string `yaml:"match"`   // exact process name; defaults to Name
	Cmdline string `yaml:"cmdline"` // or: regular expression on the command line
	Enabled *bool  `yaml:"enabled"`
}

// Systemd reports the state of systemd units.
type Systemd struct {
	Enabled bool `yaml:"enabled"`
	// Units are glob patterns of units to report individually. The number
	// of failed units is always reported.
	Units []string `yaml:"units"`
}

// Cluster collects the state of a whole Kubernetes cluster's objects (nodes,
// deployments, jobs, ...) and its events. It is meant for exactly one agent
// per cluster, run as a Deployment next to the per-node DaemonSet.
type Cluster struct {
	Enabled  bool     `yaml:"enabled"`
	Interval Duration `yaml:"interval"` // default 30s
	Events   *bool    `yaml:"events"`   // default true
}

// GPU reports graphics processors (NVIDIA through nvidia-smi, AMD through
// sysfs). It is on by itself wherever one is found.
type GPU struct {
	Enabled *bool `yaml:"enabled"` // default: on if the host has a GPU that can be read
	// Processes adds which processes, users and containers hold GPU memory. Default true.
	Processes *bool `yaml:"processes"`
	// Interval between readings; default the collection interval, at least 15s.
	Interval Duration `yaml:"interval"`
}

// OTLP opens a local endpoint that applications instrumented with
// OpenTelemetry send their metrics and logs to (OTLP over HTTP).
type OTLP struct {
	Enabled bool   `yaml:"enabled"`
	Listen  string `yaml:"listen"` // default 127.0.0.1:4318
	// Traces: every span is counted into request, error and duration
	// metrics; the spans themselves are stored for the traces that are kept.
	Traces struct {
		Enabled *bool `yaml:"enabled"` // default true
		// Sample is the share of traces whose spans are stored, 0 to 1;
		// default 1. Failed spans are always stored.
		Sample *float64 `yaml:"sample"`
		// SlowerThan: spans at least this slow are stored whatever Sample says.
		SlowerThan Duration `yaml:"slower_than"`
	} `yaml:"traces"`
	Metrics struct {
		// DropLabels removes attributes from every metric point before it
		// is stored, by label name as stored (session.id is session_id).
		// A map so that profiles add to it; false keeps a label another
		// profile dropped. Points that differ only in a dropped label are
		// added up when they are differences (delta temporality), which is
		// what tools that put a session id on every point send.
		DropLabels map[string]bool `yaml:"drop_labels"`
	} `yaml:"metrics"`
}

// Redact masks secrets in what the agent sends: the message and fields of
// every log line, from every source, and the attributes and events of
// stored spans. It runs on the agent, so a secret never reaches the
// server or the stores.
type Redact struct {
	// Builtin masks common credentials: private keys, cloud and API keys,
	// bearer tokens, passwords in URLs, and values named password, secret,
	// token or api_key. Default true.
	Builtin *bool        `yaml:"builtin"`
	Rules   []RedactRule `yaml:"rules"`
}

// RedactRule replaces what Match finds with Replace, in which $1 and
// ${name} refer to groups of Match. Replace defaults to [redacted].
type RedactRule struct {
	Name    string `yaml:"name"`
	Match   string `yaml:"match"`
	Replace string `yaml:"replace"`
	Enabled *bool  `yaml:"enabled"`
}

// Limits protect the stores from one host. What goes over is dropped on
// the agent and counted in vigil_agent_dropped_total.
type Limits struct {
	MaxSeries            int `yaml:"max_series"`               // distinct metric series; default 100000
	MaxLogBytesPerSecond int `yaml:"max_log_bytes_per_second"` // averaged over a minute; default 2 MiB
}

// Collection is what an agent collects. It can come from the agent's own
// file or be managed centrally on the server (Fleet).
type Collection struct {
	Interval    Duration          `yaml:"interval"`
	Labels      map[string]string `yaml:"labels"`
	HostMetrics *bool             `yaml:"host_metrics"`
	Scrape      Scrape            `yaml:"scrape"`
	Logs        Logs              `yaml:"logs"`
	Docker      Docker            `yaml:"docker"`
	Security    Security          `yaml:"security"`
	Probes      []Probe           `yaml:"probes"`
	Processes   []ProcessGroup    `yaml:"processes"`
	Systemd     Systemd           `yaml:"systemd"`
	GPU         GPU               `yaml:"gpu"`
	OTLP        OTLP              `yaml:"otlp"`
	Redact      Redact            `yaml:"redact"`
	Limits      Limits            `yaml:"limits"`
	Cluster     Cluster           `yaml:"cluster"`
}

type Agent struct {
	Server struct {
		URL   string `yaml:"url"`
		Token string `yaml:"token"`
		// EnrollToken is used instead of Token on hosts that enrol
		// themselves: on first start the agent exchanges it for a token of
		// its own, which it keeps in its spool directory.
		EnrollToken string `yaml:"enroll_token"`
		// How to trust the server's certificate, when it is not from a
		// publicly trusted authority: a file, or the fingerprint of the
		// server's own authority.
		CAFile      string `yaml:"ca_file"`
		CAPin       string `yaml:"ca_pin"`
		TLSInsecure bool   `yaml:"tls_insecure_skip_verify"`
	} `yaml:"server"`
	AgentID       string `yaml:"agent_id"`
	SpoolDir      string `yaml:"spool_dir"`
	SpoolMaxBytes int64  `yaml:"spool_max_bytes"`
	// RemoteConfig lets the server replace the Collection below with one
	// managed centrally. Default true.
	RemoteConfig *bool `yaml:"remote_config"`
	Collection   `yaml:",inline"`

	// LocalLabels are the labels written in the agent's own file. They
	// identify the host (env, role, ...): the server selects profiles by
	// them, and they stay on the data whatever the managed config says.
	LocalLabels map[string]string `yaml:"-"`
}

var itemNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// Off reports whether an optional "enabled" flag is set to false.
func Off(enabled *bool) bool { return enabled != nil && !*enabled }

// Check validates the parts of a collection that need no other package:
// job names, intervals and the keep/drop expressions.
func (c *Collection) Check() error {
	if v := c.OTLP.Traces.Sample; v != nil && (*v < 0 || *v > 1) {
		return fmt.Errorf("otlp.traces.sample is a share between 0 and 1, not %v", *v)
	}
	seen := map[string]bool{}
	for i, j := range c.Scrape.Static {
		where := fmt.Sprintf("scrape.static[%d]", i)
		if j.Job == "" {
			return fmt.Errorf("%s: job is required", where)
		}
		where = "scrape job " + j.Job
		if seen[j.Job] {
			return fmt.Errorf("%s is defined twice", where)
		}
		seen[j.Job] = true
		if Off(j.Enabled) {
			continue
		}
		if len(j.Targets) == 0 {
			return fmt.Errorf("%s: targets is empty", where)
		}
		if j.Interval != 0 && j.Interval.Std() < time.Second {
			return fmt.Errorf("%s: interval must be at least 1s", where)
		}
		if n := btoi(j.BearerToken != "") + btoi(j.BearerTokenFile != "") + btoi(j.BasicAuth != nil); n > 1 {
			return fmt.Errorf("%s: set only one of bearer_token, bearer_token_file and basic_auth", where)
		}
		for _, e := range append(append([]string{}, j.Keep...), j.Drop...) {
			if _, err := regexp.Compile(e); err != nil {
				return fmt.Errorf("%s: keep/drop %q: %v", where, e, err)
			}
		}
	}
	probes := map[string]bool{}
	for i, p := range c.Probes {
		if !itemNameRe.MatchString(p.Name) {
			return fmt.Errorf("probes[%d]: name is required (letters, digits, . _ -)", i)
		}
		if probes[p.Name] {
			return fmt.Errorf("probe %s is defined twice", p.Name)
		}
		probes[p.Name] = true
		if Off(p.Enabled) {
			continue
		}
		set := 0
		for _, v := range []string{p.HTTP, p.TCP, p.TLS, p.Ping} {
			if v != "" {
				set++
			}
		}
		switch {
		case set != 1:
			return fmt.Errorf("probe %s: set exactly one of http, tcp, tls and ping", p.Name)
		case p.HTTP != "" && !strings.HasPrefix(p.HTTP, "http://") && !strings.HasPrefix(p.HTTP, "https://"):
			return fmt.Errorf("probe %s: http must be a URL starting with http:// or https://", p.Name)
		case p.Interval != 0 && p.Interval.Std() < time.Second:
			return fmt.Errorf("probe %s: interval must be at least 1s", p.Name)
		case p.ExpectBody != "" && p.HTTP == "":
			return fmt.Errorf("probe %s: expect_body is for http probes", p.Name)
		}
		if p.ExpectBody != "" {
			if _, err := regexp.Compile(p.ExpectBody); err != nil {
				return fmt.Errorf("probe %s: expect_body: %v", p.Name, err)
			}
		}
		if p.TLS != "" {
			if _, _, err := net.SplitHostPort(p.TLS); err != nil {
				return fmt.Errorf("probe %s: tls must be host:port", p.Name)
			}
		}
	}
	groups := map[string]bool{}
	for i, g := range c.Processes {
		if !itemNameRe.MatchString(g.Name) {
			return fmt.Errorf("processes[%d]: name is required (letters, digits, . _ -)", i)
		}
		if groups[g.Name] {
			return fmt.Errorf("process group %s is defined twice", g.Name)
		}
		groups[g.Name] = true
		if g.Match != "" && g.Cmdline != "" {
			return fmt.Errorf("process group %s: set match or cmdline, not both", g.Name)
		}
		if _, err := regexp.Compile(g.Cmdline); err != nil {
			return fmt.Errorf("process group %s: cmdline: %v", g.Name, err)
		}
	}
	for _, u := range c.Systemd.Units {
		if _, err := filepath.Match(u, ""); err != nil {
			return fmt.Errorf("systemd.units: bad pattern %q", u)
		}
	}
	if c.Limits.MaxSeries < 0 || c.Limits.MaxLogBytesPerSecond < 0 {
		return fmt.Errorf("limits cannot be negative")
	}
	names := map[string]bool{}
	drops := map[string]bool{}
	for i, d := range c.Logs.Drop {
		if !itemNameRe.MatchString(d.Name) {
			return fmt.Errorf("logs.drop[%d]: name is required (letters, digits, . _ -)", i)
		}
		if drops[d.Name] {
			return fmt.Errorf("logs.drop %s is defined twice", d.Name)
		}
		drops[d.Name] = true
		if Off(d.Enabled) {
			continue
		}
		if d.Container == "" && d.Unit == "" && d.Pod == "" && d.App == "" {
			return fmt.Errorf("logs.drop %s: say whose lines, with container, unit, pod or app", d.Name)
		}
		for _, pattern := range []string{d.Container, d.Unit, d.Pod, d.App} {
			if _, err := path.Match(pattern, ""); err != nil {
				return fmt.Errorf("logs.drop %s: %q is not a valid pattern", d.Name, pattern)
			}
		}
		if _, err := regexp.Compile(d.Match); err != nil {
			return fmt.Errorf("logs.drop %s: match: %v", d.Name, err)
		}
	}
	for _, f := range c.Logs.Files {
		if f.Name != "" && names[f.Name] {
			return fmt.Errorf("log source %s is defined twice", f.Name)
		}
		names[f.Name] = true
	}
	redacts := map[string]bool{}
	for i, r := range c.Redact.Rules {
		if !itemNameRe.MatchString(r.Name) {
			return fmt.Errorf("redact.rules[%d]: name is required (letters, digits, . _ -)", i)
		}
		if redacts[r.Name] {
			return fmt.Errorf("redact rule %s is defined twice", r.Name)
		}
		redacts[r.Name] = true
		if Off(r.Enabled) {
			continue
		}
		if r.Match == "" {
			return fmt.Errorf("redact rule %s: match is required", r.Name)
		}
		if _, err := regexp.Compile(r.Match); err != nil {
			return fmt.Errorf("redact rule %s: match: %v", r.Name, err)
		}
	}
	return nil
}

// defaults fills in what a Collection leaves unset.
func (c *Collection) defaults() {
	t := true
	if c.Interval == 0 {
		c.Interval = Duration(15 * time.Second)
	}
	if c.HostMetrics == nil {
		c.HostMetrics = &t
	}
	if c.Docker.Socket == "" {
		c.Docker.Socket = "/var/run/docker.sock"
	}
	if c.Docker.Logs == nil {
		c.Docker.Logs = &t
	}
	if c.Docker.Metrics == nil {
		c.Docker.Metrics = &t
	}
	if c.Logs.Kubernetes.Path == "" {
		c.Logs.Kubernetes.Path = "/var/log/pods"
	}
	if c.Cluster.Interval == 0 {
		c.Cluster.Interval = Duration(30 * time.Second)
	}
	if c.Cluster.Events == nil {
		c.Cluster.Events = &t
	}
	if c.OTLP.Traces.Enabled == nil {
		on := true
		c.OTLP.Traces.Enabled = &on
	}
	if c.OTLP.Traces.Sample == nil {
		one := 1.0
		c.OTLP.Traces.Sample = &one
	}
	if c.OTLP.Listen == "" {
		c.OTLP.Listen = "127.0.0.1:4318"
	}
	if c.Limits.MaxSeries == 0 {
		c.Limits.MaxSeries = 100000
	}
	if c.Limits.MaxLogBytesPerSecond == 0 {
		c.Limits.MaxLogBytesPerSecond = 2 << 20
	}
	sec := &c.Security
	if sec.CaptureBefore == 0 {
		sec.CaptureBefore = Duration(30 * time.Second)
	}
	if sec.CaptureAfter == 0 {
		sec.CaptureAfter = Duration(30 * time.Second)
	}
	if sec.CaptureCooldown == 0 {
		sec.CaptureCooldown = Duration(5 * time.Minute)
	}
	// The buffer must still hold the "before" part when a capture ends.
	if min := sec.CaptureBefore + sec.CaptureAfter + Duration(10*time.Second); sec.Buffer < min {
		sec.Buffer = min
	}
}

// Hash identifies a managed config text. The server computes it for the
// config an agent should run, the agent for the one it does run.
func Hash(text string) string {
	h := sha256.Sum256([]byte(text))
	return hex.EncodeToString(h[:8])
}

// ParseCollection reads a centrally managed collection config. ${VAR} is
// left alone here: the text comes from the server, not from a local file.
func ParseCollection(text string) (*Collection, error) {
	c := &Collection{}
	dec := yaml.NewDecoder(strings.NewReader(text))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil && err != io.EOF {
		return nil, err
	}
	if c.Interval != 0 && c.Interval.Std() < time.Second {
		return nil, fmt.Errorf("interval must be at least 1s")
	}
	c.defaults()
	return c, nil
}

type Server struct {
	Listen  string `yaml:"listen"`
	DataDir string `yaml:"data_dir"`
	// ExternalURL is how people reach this server; it is used for links in
	// notifications.
	ExternalURL string `yaml:"external_url"`
	// NotifySilences tells the contacts of the alerts concerned when a
	// silence is made, and when an alert they were told about ends while
	// silenced. On unless set to false.
	NotifySilences *bool `yaml:"notify_silences"`
	Storage        struct {
		MetricsURL string `yaml:"metrics_url"`
		LogsURL    string `yaml:"logs_url"`
		// TracesURL is a VictoriaTraces instance; optional. Without it,
		// spans are counted into metrics but not kept.
		TracesURL string `yaml:"traces_url"`
	} `yaml:"storage"`
	// TLS encrypts the server's port. Either give a certificate and key
	// (from a public or company authority), or let the server be its own
	// authority: it then makes and renews its certificate itself, and
	// agents trust it by the authority's fingerprint.
	TLS struct {
		CertFile   string   `yaml:"cert_file"`
		KeyFile    string   `yaml:"key_file"`
		SelfSigned bool     `yaml:"self_signed"`
		Names      []string `yaml:"names"` // further names and addresses for the certificate
	} `yaml:"tls"`
	// CapturesMaxBytes caps the disk used by stored captures; the oldest are
	// removed first. Default 2 GiB.
	// Costs are the prices usage is multiplied with: tokens of language
	// models and hours of GPUs. Without them usage is still counted, just
	// not priced.
	Costs Costs `yaml:"costs"`
	// TrustedProxies are the addresses (or networks) of reverse proxies in
	// front of the server. Only from them is X-Forwarded-For believed, so
	// that sign-in limits and the addresses shown are those of the real
	// clients and not of the proxy.
	TrustedProxies []string `yaml:"trusted_proxies"`
	// Limits protect the server and the stores from one agent that sends
	// far more than it should, and from too much arriving at once.
	Limits           ServerLimits `yaml:"limits"`
	CapturesMaxBytes int64        `yaml:"captures_max_bytes"`
	IngestToken      string       `yaml:"ingest_token"`
	AdminToken       string       `yaml:"admin_token"`
	HeartbeatTimeout Duration     `yaml:"heartbeat_timeout"`
	// Scrape lets the server pull from hosts that run no agent.
	Interval Duration `yaml:"interval"`
	Scrape   Scrape   `yaml:"scrape"`
}

// load reads a YAML file, expanding ${VAR} from the environment so tokens
// can stay out of the file.
func load(path string, out any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := yaml.NewDecoder(strings.NewReader(os.ExpandEnv(string(b))))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func LoadAgent(path string) (*Agent, error) {
	c := &Agent{}
	if err := load(path, c); err != nil {
		return nil, err
	}
	if c.Server.URL == "" || (c.Server.Token == "" && c.Server.EnrollToken == "") {
		return nil, fmt.Errorf("%s: server.url and server.token (or server.enroll_token) are required", path)
	}
	if c.AgentID == "" {
		h, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("agent_id not set and hostname unavailable: %w", err)
		}
		c.AgentID = h
	}
	if c.SpoolDir == "" {
		c.SpoolDir = "/var/lib/vigil/spool"
	}
	if c.SpoolMaxBytes == 0 {
		c.SpoolMaxBytes = 512 << 20
	}
	if c.RemoteConfig == nil {
		t := true
		c.RemoteConfig = &t
	}
	c.LocalLabels = maps.Clone(c.Labels)
	c.defaults()
	return c, nil
}

// Costs is the server's price list. Prices are in one currency of your
// choosing; nothing converts.
type Costs struct {
	// Tokens: price per million tokens. An entry applies to every model
	// whose name starts with its model (so "claude-sonnet-4" also prices
	// "claude-sonnet-4-20250514"); the longest match wins.
	Tokens []TokenPrice `yaml:"tokens"`
	// GPUHours: price of one GPU for one hour, matched the same way against
	// the model the card reports; an entry with an empty model is the default.
	GPUHours []GPUPrice `yaml:"gpu_hours"`
}

type TokenPrice struct {
	Model      string  `yaml:"model"`
	Input      float64 `yaml:"input"`
	Output     float64 `yaml:"output"`
	CacheRead  float64 `yaml:"cache_read"`
	CacheWrite float64 `yaml:"cache_write"`
}

type GPUPrice struct {
	Model string  `yaml:"model"`
	Price float64 `yaml:"price"`
}

// IngestLimit is what one agent may send, averaged over a minute. Zero
// means the default from the file's limits section; negative, no limit.
type IngestLimit struct {
	SamplesPerSecond  int `yaml:"samples_per_second"`
	LogBytesPerSecond int `yaml:"log_bytes_per_second"`
}

type ServerLimits struct {
	IngestLimit `yaml:",inline"` // per agent; defaults 20000 samples and 4 MiB of logs a second
	// MaxInFlight bounds the batches being stored at the same time; agents
	// over it are told to try again. Default 512.
	MaxInFlight int `yaml:"max_in_flight"`
	// Agents overrides the per-agent limits for the agent ids named.
	Agents map[string]IngestLimit `yaml:"agents"`
}

// For returns the limits of one agent. Zero or less means no limit.
func (l *ServerLimits) For(agent string) IngestLimit {
	out := l.IngestLimit
	if o, ok := l.Agents[agent]; ok {
		if o.SamplesPerSecond != 0 {
			out.SamplesPerSecond = o.SamplesPerSecond
		}
		if o.LogBytesPerSecond != 0 {
			out.LogBytesPerSecond = o.LogBytesPerSecond
		}
	}
	return out
}

// Relay is the config of a relay: a process agents send to instead of the
// server, which passes everything on. It is for networks from which only
// one host may talk to the server, and for keeping the number of
// connections to the server down: one per site instead of one per agent.
type Relay struct {
	Listen string `yaml:"listen"`
	// RelayID is what the server shows next to agents that came through
	// this relay. Defaults to the host name.
	RelayID string `yaml:"relay_id"`
	// DataDir holds the relay's own certificate authority, with tls.self_signed.
	DataDir string `yaml:"data_dir"`
	TLS     struct {
		CertFile   string   `yaml:"cert_file"`
		KeyFile    string   `yaml:"key_file"`
		SelfSigned bool     `yaml:"self_signed"`
		Names      []string `yaml:"names"`
	} `yaml:"tls"`
	// Server is where everything is passed on to: the server, or another relay.
	Server struct {
		URL         string `yaml:"url"`
		CAFile      string `yaml:"ca_file"`
		CAPin       string `yaml:"ca_pin"`
		TLSInsecure bool   `yaml:"tls_insecure_skip_verify"`
	} `yaml:"server"`
	// MaxInFlight bounds the requests passed on at the same time; agents
	// over it are told to try again. Default 256.
	MaxInFlight int `yaml:"max_in_flight"`
}

func LoadRelay(path string) (*Relay, error) {
	c := &Relay{}
	if err := load(path, c); err != nil {
		return nil, err
	}
	if c.Server.URL == "" {
		return nil, fmt.Errorf("%s: server.url is required", path)
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/vigil-relay"
	}
	if c.RelayID == "" {
		h, err := os.Hostname()
		if err != nil {
			return nil, fmt.Errorf("relay_id not set and hostname unavailable: %w", err)
		}
		c.RelayID = h
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return nil, fmt.Errorf("%s: tls needs both cert_file and key_file", path)
	}
	if c.TLS.SelfSigned && c.TLS.CertFile != "" {
		return nil, fmt.Errorf("%s: tls is either self_signed or cert_file and key_file", path)
	}
	if c.MaxInFlight <= 0 {
		c.MaxInFlight = 256
	}
	return c, nil
}

func LoadServer(path string) (*Server, error) {
	c := &Server{}
	if err := load(path, c); err != nil {
		return nil, err
	}
	for i, t := range c.Costs.Tokens {
		if t.Model == "" || t.Input < 0 || t.Output < 0 || t.CacheRead < 0 || t.CacheWrite < 0 {
			return nil, fmt.Errorf("%s: costs.tokens[%d] needs a model, and prices cannot be negative", path, i)
		}
	}
	for i, g := range c.Costs.GPUHours {
		if g.Price < 0 {
			return nil, fmt.Errorf("%s: costs.gpu_hours[%d]: the price cannot be negative", path, i)
		}
	}
	for _, t := range c.TrustedProxies {
		if _, _, err := net.ParseCIDR(t); err != nil && net.ParseIP(t) == nil {
			return nil, fmt.Errorf("%s: trusted_proxies: %q is neither an address nor a network like 10.0.0.0/8", path, t)
		}
	}
	// Applied only to a config read from a file; one built in code has no limits.
	if c.Limits.SamplesPerSecond == 0 {
		c.Limits.SamplesPerSecond = 20000
	}
	if c.Limits.LogBytesPerSecond == 0 {
		c.Limits.LogBytesPerSecond = 4 << 20
	}
	if c.Limits.MaxInFlight == 0 {
		c.Limits.MaxInFlight = 512
	}
	if c.AdminToken == "" {
		return nil, fmt.Errorf("%s: admin_token is required", path)
	}
	if c.Storage.MetricsURL == "" || c.Storage.LogsURL == "" {
		return nil, fmt.Errorf("%s: storage.metrics_url and storage.logs_url are required", path)
	}
	if c.Listen == "" {
		c.Listen = ":8080"
	}
	if c.DataDir == "" {
		c.DataDir = "/var/lib/vigil"
	}
	if (c.TLS.CertFile == "") != (c.TLS.KeyFile == "") {
		return nil, fmt.Errorf("%s: tls needs both cert_file and key_file", path)
	}
	if c.TLS.SelfSigned && c.TLS.CertFile != "" {
		return nil, fmt.Errorf("%s: tls: use either self_signed or cert_file and key_file", path)
	}
	if c.ExternalURL == "" {
		scheme := "http"
		if c.TLS.SelfSigned || c.TLS.CertFile != "" {
			scheme = "https"
		}
		c.ExternalURL = scheme + "://localhost" + c.Listen[strings.LastIndex(c.Listen, ":"):]
	}
	if c.CapturesMaxBytes == 0 {
		c.CapturesMaxBytes = 2 << 30
	}
	if c.HeartbeatTimeout == 0 {
		c.HeartbeatTimeout = Duration(60 * time.Second)
	}
	if c.Interval == 0 {
		c.Interval = Duration(15 * time.Second)
	}
	return c, nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
