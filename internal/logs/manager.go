package logs

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
	"github.com/ijcodelabs/vigil/internal/dockerapi"
	"github.com/ijcodelabs/vigil/internal/kube"
	"github.com/ijcodelabs/vigil/internal/model"
)

const (
	flushEvery   = 2 * time.Second
	flushEntries = 2000
	flushBytes   = 1 << 20
)

// Manager owns the log sources of one agent and batches what they produce.
type Manager struct {
	emit   func([]model.LogEntry) error
	labels map[string]string
	log    *slog.Logger
	pos    *Positions
	recs   chan Record
	run    []func(context.Context)
}

// NewManager builds the sources the config asks for. emit receives each
// batch; positions only advance when it returns nil. docker may be nil.
func NewManager(cfg *config.Collection, stateDir string, docker *dockerapi.Client, pods *kube.Client, emit func([]model.LogEntry) error, log *slog.Logger) (*Manager, error) {
	templates := map[string]*Template{}
	for name, tc := range cfg.Logs.Templates {
		t, err := Compile(name, tc)
		if err != nil {
			return nil, err
		}
		templates[name] = t
	}
	lookup := func(where, name string) (*Template, error) {
		if name == "" {
			return &Template{}, nil
		}
		if t, ok := templates[name]; ok {
			return t, nil
		}
		return nil, fmt.Errorf("%s: unknown log template %q", where, name)
	}
	pos, err := LoadPositions(filepath.Join(stateDir, "positions.json"))
	if err != nil {
		return nil, fmt.Errorf("positions: %w", err)
	}
	m := &Manager{emit: emit, labels: cfg.Labels, log: log, pos: pos, recs: make(chan Record, 4096)}

	var files []FileSource
	for i, f := range cfg.Logs.Files {
		if config.Off(f.Enabled) {
			continue
		}
		t, err := lookup(fmt.Sprintf("logs.files[%d]", i), f.Template)
		if err != nil {
			return nil, err
		}
		files = append(files, FileSource{Paths: f.Paths, Exclude: f.Exclude, Template: t,
			Labels: f.Labels, FromBeginning: f.FromBeginning})
	}
	if k := cfg.Logs.Kubernetes; k.Enabled {
		byContainer := map[string]*Template{}
		for container, name := range k.ContainerTemplates {
			t, err := lookup("logs.kubernetes.container_templates", name)
			if err != nil {
				return nil, err
			}
			byContainer[container] = t
		}
		files = append(files, FileSource{
			Paths:    []string{filepath.Join(k.Path, "*", "*", "*.log")},
			Format:   formatCRI,
			Template: &Template{},
			Labels:   k.Labels,
			PerFile: func(path string) (map[string]string, *Template, bool) {
				l, ok := podLogLabels(path)
				if !ok {
					return nil, nil, false
				}
				tmpl := byContainer[l["container"]]
				if pods == nil {
					return l, tmpl, true
				}
				// The pod itself can say how its logs are read, or that they are not.
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				pod, found := pods.Pod(ctx, l["namespace"], l["pod"])
				if !found {
					return l, tmpl, true
				}
				if pod.Annotations["vigil.io/logs"] == "false" {
					return nil, nil, false
				}
				if name := pod.Annotations["vigil.io/log-template"]; name != "" {
					if t, ok := templates[name]; ok {
						tmpl = t
					} else {
						log.Warn("pod asks for an unknown log template", "pod", l["namespace"]+"/"+l["pod"], "template", name)
					}
				}
				for _, key := range []string{"app.kubernetes.io/name", "app"} {
					if app := pod.Labels[key]; app != "" {
						l["app"] = app
						break
					}
				}
				return l, tmpl, true
			},
		})
	}
	if len(files) > 0 {
		t := &Tailer{Sources: files, Pos: pos, Out: m.recs, Log: log}
		m.run = append(m.run, t.Run)
	}
	if j := cfg.Logs.Journald; j.Enabled {
		t, err := lookup("logs.journald", j.Template)
		if err != nil {
			return nil, err
		}
		src := &Journald{Units: j.Units, Template: t, Templates: templates, ByUnit: j.UnitTemplates, Labels: j.Labels,
			Pos: pos, Out: m.recs, Log: log}
		m.run = append(m.run, src.Run)
	}
	if docker != nil && cfg.Docker.Enabled && *cfg.Docker.Logs {
		t, err := lookup("docker", cfg.Docker.Template)
		if err != nil {
			return nil, err
		}
		src := &Docker{Client: docker, Default: t, Templates: templates, ByContainer: cfg.Docker.ContainerTemplates, Labels: cfg.Docker.Labels,
			Pos: pos, Out: m.recs, Log: log}
		m.run = append(m.run, src.Run)
	}
	return m, nil
}

// Validate checks a collection's log section without touching any state:
// every template compiles and every reference names a template that exists.
func Validate(cfg *config.Collection) error {
	templates := map[string]bool{"": true}
	for name, tc := range cfg.Logs.Templates {
		if _, err := Compile(name, tc); err != nil {
			return err
		}
		templates[name] = true
	}
	refs := map[string]string{"logs.journald": cfg.Logs.Journald.Template, "docker": cfg.Docker.Template}
	for i, f := range cfg.Logs.Files {
		refs[fmt.Sprintf("logs.files[%d]", i)] = f.Template
		if len(f.Paths) == 0 {
			return fmt.Errorf("logs.files[%d]: paths is empty", i)
		}
		for _, p := range append(append([]string{}, f.Paths...), f.Exclude...) {
			if _, err := filepath.Match(p, ""); err != nil {
				return fmt.Errorf("logs.files[%d]: bad pattern %q", i, p)
			}
		}
	}
	for container, name := range cfg.Logs.Kubernetes.ContainerTemplates {
		refs["logs.kubernetes.container_templates."+container] = name
	}
	for pattern, name := range cfg.Logs.Journald.UnitTemplates {
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("logs.journald.unit_templates: bad pattern %q", pattern)
		}
		refs["logs.journald.unit_templates."+pattern] = name
	}
	for pattern, name := range cfg.Docker.ContainerTemplates {
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("docker.container_templates: bad pattern %q", pattern)
		}
		refs["docker.container_templates."+pattern] = name
	}
	for where, name := range refs {
		if !templates[name] {
			return fmt.Errorf("%s: unknown log template %q", where, name)
		}
	}
	return nil
}

// Active reports whether any log source is configured.
func (m *Manager) Active() bool { return len(m.run) > 0 }

// Run blocks until ctx is cancelled, then ships what is already queued.
func (m *Manager) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, run := range m.run {
		wg.Add(1)
		go func() { defer wg.Done(); run(ctx) }()
	}

	var (
		batch   []model.LogEntry
		pending []Record // positions to commit with this batch
		size    int
	)
	flush := func() {
		if len(batch) > 0 {
			if err := m.emit(batch); err != nil {
				// Not spooled: leave positions alone so a restart re-reads these lines.
				m.log.Error("log batch lost", "entries", len(batch), "err", err)
				batch, pending, size = nil, nil, 0
				return
			}
		}
		for _, r := range pending {
			if r.Delete {
				m.pos.Delete(r.Key)
			} else {
				m.pos.Set(r.Key, r.Pos)
			}
		}
		if err := m.pos.Save(); err != nil {
			m.log.Error("saving log positions failed", "err", err)
		}
		batch, pending, size = nil, nil, 0
	}
	add := func(r Record) {
		if r.Entry != nil {
			for k, v := range m.labels {
				r.Entry.Stream[k] = v
			}
			batch = append(batch, *r.Entry)
			size += len(r.Entry.Msg) + 64
		}
		if r.Key != "" && (r.Pos != "" || r.Delete) {
			pending = append(pending, Record{Key: r.Key, Pos: r.Pos, Delete: r.Delete})
		}
	}

	tick := time.NewTicker(flushEvery)
	defer tick.Stop()
	for {
		select {
		case r := <-m.recs:
			add(r)
			if len(batch) >= flushEntries || size >= flushBytes {
				flush()
			}
		case <-tick.C:
			flush()
		case <-ctx.Done():
			wg.Wait()
			for {
				select {
				case r := <-m.recs:
					add(r)
					continue
				default:
				}
				break
			}
			flush()
			return
		}
	}
}
