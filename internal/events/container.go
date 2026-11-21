package events

import (
	"context"
	"path"
	"slices"
	"strings"
	"time"
)

// ContainerSetup is how a container was started, as far as it matters for
// security. The agent fills it from Docker or from the node's pod list.
type ContainerSetup struct {
	ID, Name, Image string
	Privileged      bool
	HostPID         bool // shares the host's process namespace
	HostNetwork     bool
	HostIPC         bool
	CapAdd          []string // capabilities added to the default set
	RunsAsRoot      bool
	ReadOnlyRoot    bool
	Mounts          []HostMount // host paths mounted into the container
}

// HostMount is a host file or directory mounted into a container.
type HostMount struct {
	Path     string
	Writable bool
}

// runtimeSockets are the container runtimes' API sockets. Whoever can reach
// one can start a privileged container, so mounting one is as good as root
// on the host.
var runtimeSockets = []string{"docker.sock", "containerd.sock", "crio.sock", "podman.sock", "cri-dockerd.sock"}

// socketDirs hold a runtime socket; mounting the directory mounts the socket.
var socketDirs = []string{"/run", "/var/run", "/run/containerd", "/var/run/containerd", "/run/crio", "/var/run/crio",
	"/run/podman", "/var/run/podman", "/run/docker", "/var/run/docker"}

// runtimeSocket returns the first mounted host path that is, or holds, a
// container runtime's socket.
func runtimeSocket(mounts []HostMount) string {
	for _, m := range mounts {
		p := path.Clean(m.Path)
		if slices.Contains(runtimeSockets, path.Base(p)) || slices.Contains(socketDirs, p) {
			return m.Path
		}
	}
	return ""
}

// normalCaps writes capabilities the same way whatever the source said:
// Docker accepts "CAP_SYS_ADMIN" and "sys_admin", Kubernetes "SYS_ADMIN".
func normalCaps(caps []string) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, strings.TrimPrefix(strings.ToUpper(c), "CAP_"))
	}
	return out
}

// event turns a setup into the `container` event the rules see.
func (s ContainerSetup) event(now time.Time) *Event {
	ev := &Event{Time: now, Type: TypeContainer, Container: s.Name, ContainerID: s.ID, Image: s.Image,
		Privileged: s.Privileged, HostPID: s.HostPID, HostNetwork: s.HostNetwork, HostIPC: s.HostIPC,
		CapAdd: normalCaps(s.CapAdd), RunsAsRoot: s.RunsAsRoot, ReadOnlyRoot: s.ReadOnlyRoot,
		RuntimeSocket: runtimeSocket(s.Mounts)}
	if len(ev.ContainerID) > 12 {
		ev.ContainerID = ev.ContainerID[:12]
	}
	if ev.Container == "" {
		ev.Container = ev.ContainerID
	}
	for _, m := range s.Mounts {
		ev.HostMounts = append(ev.HostMounts, m.Path)
		if m.Writable {
			ev.HostMountsRW = append(ev.HostMountsRW, m.Path)
		}
	}
	return ev
}

// ContainerSource lists the running containers and tells how one was
// started. The agent implements it over Docker and the node's pod list.
type ContainerSource interface {
	// Running returns the running containers' full ids: empty when there
	// are none, nil when no runtime could be asked.
	Running(ctx context.Context) []string
	Setup(ctx context.Context, id string) (ContainerSetup, bool)
}

// containerWatch reports each container once: the running ones when it
// starts, then every one that starts later.
type containerWatch struct {
	src   ContainerSource
	known map[string]bool
}

const containerEvery = 10 * time.Second

// poll returns the events of containers not seen before and forgets the
// ones that are gone, so a container started again is reported again.
func (w *containerWatch) poll(ctx context.Context, now time.Time) []*Event {
	running := w.src.Running(ctx)
	if running == nil {
		return nil // the runtime did not answer: keep what is known
	}
	alive := make(map[string]bool, len(running))
	var out []*Event
	for _, id := range running {
		alive[id] = true
		if w.known[id] {
			continue
		}
		s, ok := w.src.Setup(ctx, id)
		if !ok {
			continue // try again next time
		}
		w.known[id] = true
		if s.ID == "" {
			s.ID = id
		}
		out = append(out, s.event(now))
	}
	for id := range w.known {
		if !alive[id] {
			delete(w.known, id)
		}
	}
	return out
}

// watchContainers polls src until ctx ends and sends the new containers'
// events on out.
func watchContainers(ctx context.Context, src ContainerSource, out chan<- []*Event) {
	w := &containerWatch{src: src, known: map[string]bool{}}
	tick := time.NewTicker(containerEvery)
	defer tick.Stop()
	for {
		if evs := w.poll(ctx, time.Now()); len(evs) > 0 {
			select {
			case out <- evs:
			case <-ctx.Done():
				return
			}
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			return
		}
	}
}
