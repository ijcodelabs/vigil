package events

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
)

func TestRuntimeSocket(t *testing.T) {
	for _, c := range []struct {
		mounts []HostMount
		want   string
	}{
		{[]HostMount{{Path: "/var/run/docker.sock"}}, "/var/run/docker.sock"},
		{[]HostMount{{Path: "/srv/data"}, {Path: "/run/containerd/containerd.sock"}}, "/run/containerd/containerd.sock"},
		{[]HostMount{{Path: "/var/run/"}}, "/var/run/"}, // the directory holds the socket
		{[]HostMount{{Path: "/etc/localtime"}, {Path: "/srv/run"}}, ""},
		{nil, ""},
	} {
		if got := runtimeSocket(c.mounts); got != c.want {
			t.Errorf("runtimeSocket(%v) = %q, want %q", c.mounts, got, c.want)
		}
	}
	if got := normalCaps([]string{"CAP_SYS_ADMIN", "net_admin", "SYS_PTRACE"}); !slices.Equal(got, []string{"SYS_ADMIN", "NET_ADMIN", "SYS_PTRACE"}) {
		t.Errorf("normalCaps = %v", got)
	}
}

type fakeContainers struct {
	running []string
	setups  map[string]ContainerSetup
	asked   []string
}

func (f *fakeContainers) Running(context.Context) []string { return f.running }
func (f *fakeContainers) Setup(_ context.Context, id string) (ContainerSetup, bool) {
	f.asked = append(f.asked, id)
	s, ok := f.setups[id]
	return s, ok
}

func TestContainerWatch(t *testing.T) {
	id := strings.Repeat("ab", 32)
	f := &fakeContainers{running: []string{id}, setups: map[string]ContainerSetup{
		id: {Name: "shop-api", Image: "acme/api:1", Privileged: true, CapAdd: []string{"cap_sys_admin"},
			Mounts: []HostMount{{Path: "/", Writable: false}, {Path: "/var/run/docker.sock", Writable: true}}}}}
	w := &containerWatch{src: f, known: map[string]bool{}}
	ctx, now := context.Background(), time.Now()

	evs := w.poll(ctx, now)
	if len(evs) != 1 {
		t.Fatalf("first poll: %d events", len(evs))
	}
	ev := evs[0]
	if ev.Type != TypeContainer || ev.Container != "shop-api" || ev.ContainerID != id[:12] || !ev.Privileged ||
		!slices.Equal(ev.CapAdd, []string{"SYS_ADMIN"}) || !slices.Equal(ev.HostMounts, []string{"/", "/var/run/docker.sock"}) ||
		!slices.Equal(ev.HostMountsRW, []string{"/var/run/docker.sock"}) || ev.RuntimeSocket != "/var/run/docker.sock" {
		t.Errorf("event: %+v", ev)
	}
	if evs := w.poll(ctx, now); len(evs) != 0 || len(f.asked) != 1 {
		t.Errorf("a known container is reported again (%d) or asked about again (%v)", len(evs), f.asked)
	}
	f.running = nil // the runtime did not answer: nothing is forgotten
	w.poll(ctx, now)
	f.running = []string{id}
	if evs := w.poll(ctx, now); len(evs) != 0 {
		t.Error("a runtime that did not answer made a container look new")
	}
	f.running = []string{} // gone, then started again
	w.poll(ctx, now)
	f.running = []string{id}
	if evs := w.poll(ctx, now); len(evs) != 1 {
		t.Errorf("a container started again: %d events, want 1", len(evs))
	}
}

func TestContainerRules(t *testing.T) {
	e, err := NewEngine(security(t,
		config.SecurityRule{Name: "privileged_container", When: `type == "container" && privileged`,
			Summary: "{container} ({image}) runs privileged"},
		config.SecurityRule{Name: "runtime_socket_mounted", When: `type == "container" && runtime_socket != ""`},
		config.SecurityRule{Name: "host_root_mounted", When: `type == "container" && host_mounts.exists(m, m == "/")`},
		config.SecurityRule{Name: "sys_admin", When: `type == "container" && "SYS_ADMIN" in cap_add`},
		config.SecurityRule{Name: "shell", When: `type == "exec" && privileged`}, // fields of other types are false
	))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	e.OnDetection = func(d Detection) { got = append(got, d.Rule+": "+d.Summary) }
	s := ContainerSetup{ID: strings.Repeat("c", 64), Name: "shop-api", Image: "acme/api:1", Privileged: true,
		CapAdd: []string{"SYS_ADMIN"}, Mounts: []HostMount{{Path: "/"}}}
	e.Handle(s.event(time.Now()))
	e.Handle(ContainerSetup{ID: strings.Repeat("d", 64), Name: "quiet"}.event(time.Now()))
	e.Handle(&Event{Time: time.Now(), Type: TypeExec, Exe: "/bin/sh", Comm: "sh"})
	want := "privileged_container: shop-api (acme/api:1) runs privileged|host_root_mounted: shop-api started from acme/api:1|" +
		"sys_admin: shop-api started from acme/api:1"
	if strings.Join(got, "|") != want {
		t.Errorf("detections:\n got %q\nwant %q", strings.Join(got, "|"), want)
	}
}

// Without kernel probes the monitor still runs the rules on container events.
func TestMonitorWithoutProbes(t *testing.T) {
	cfg := security(t, config.SecurityRule{Name: "privileged_container", When: `type == "container" && privileged`})
	engine, err := NewEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("e", 64)
	m := NewMonitor(cfg, slog.New(slog.DiscardHandler))
	m.Containers = &fakeContainers{running: []string{id}, setups: map[string]ContainerSetup{id: {Name: "x", Privileged: true}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan []Detection, 1)
	m.SendDetections = func(ds []Detection) error { got <- ds; cancel(); return nil }
	go m.run(ctx, engine, nil)
	select {
	case ds := <-got:
		if len(ds) != 1 || ds[0].Rule != "privileged_container" || ds[0].Event.Container != "x" {
			t.Errorf("detections: %+v", ds)
		}
	case <-ctx.Done():
		t.Fatal("no detection from a container event without kernel probes")
	}
}

// A container whose first process runs before the next cgroup scan is
// still recognised once a scan has seen its cgroup.
func TestNewContainerCgroup(t *testing.T) {
	root := t.TempDir()
	c := newContainers(nil)
	c.root, c.rescan = root, 50*time.Millisecond
	c.scan()
	id := strings.Repeat("f", 64)
	dir := filepath.Join(root, "system.slice", "docker-"+id+".scope")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	ino := st.Sys().(*syscall.Stat_t).Ino
	ev := &Event{cgroup: uint64(ino)}
	c.apply(ev) // right after a scan: not looked up yet
	time.Sleep(60 * time.Millisecond)
	ev = &Event{cgroup: uint64(ino)}
	c.apply(ev)
	if ev.ContainerID != id[:12] {
		t.Errorf("the new container's cgroup stayed unknown: %+v", ev)
	}
}
