package agent

import (
	"context"
	"strings"

	"github.com/ijcodelabs/vigil/internal/dockerapi"
	"github.com/ijcodelabs/vigil/internal/events"
	"github.com/ijcodelabs/vigil/internal/kube"
)

// containerSource tells the runtime-event monitor which containers run and
// how they were started, from Docker and from the node's pod list.
type containerSource struct {
	docker *dockerapi.Client // nil when the Docker integration is off
	pods   *kube.Client      // nil off Kubernetes
}

func (c containerSource) Running(ctx context.Context) []string {
	var ids []string
	answered := false
	if c.docker != nil {
		if list, err := c.docker.List(ctx); err == nil {
			answered = true
			for _, ct := range list {
				if ct.State == "running" {
					ids = append(ids, ct.ID)
				}
			}
		}
	}
	if c.pods != nil {
		if list, err := c.pods.Pods(ctx); err == nil {
			answered = true
			for _, p := range list {
				for _, ct := range p.Containers {
					if ct.State == "running" && ct.ID != "" {
						ids = append(ids, ct.ID)
					}
				}
			}
		}
	}
	if answered && ids == nil {
		ids = []string{}
	}
	return ids
}

func (c containerSource) Setup(ctx context.Context, id string) (events.ContainerSetup, bool) {
	if c.pods != nil {
		list, _ := c.pods.Pods(ctx)
		for _, p := range list {
			for _, ct := range p.Containers {
				if ct.ID == id {
					return podSetup(p, ct), true
				}
			}
		}
	}
	if c.docker == nil {
		return events.ContainerSetup{}, false
	}
	s, err := c.docker.Setup(ctx, id)
	if err != nil {
		return events.ContainerSetup{}, false
	}
	name := ""
	if list, err := c.docker.List(ctx); err == nil {
		for _, ct := range list {
			if ct.ID == id {
				name = ct.Name()
			}
		}
	}
	return dockerSetup(id, name, s), true
}

func dockerSetup(id, name string, s dockerapi.Setup) events.ContainerSetup {
	out := events.ContainerSetup{ID: id, Name: name, Image: s.Image, Privileged: s.Privileged,
		HostPID: s.PidMode == "host", HostNetwork: s.NetworkMode == "host", HostIPC: s.IpcMode == "host",
		CapAdd: s.CapAdd, RunsAsRoot: isRootUser(s.User), ReadOnlyRoot: s.ReadonlyRootfs}
	for _, b := range s.Binds {
		out.Mounts = append(out.Mounts, events.HostMount{Path: b.Source, Writable: b.RW})
	}
	return out
}

// isRootUser reads Docker's user setting: "", a name or uid, either with
// an optional ":group". Without one the container runs as root.
func isRootUser(user string) bool {
	u, _, _ := strings.Cut(user, ":")
	return u == "" || u == "0" || u == "root"
}

func podSetup(p kube.Pod, ct kube.Container) events.ContainerSetup {
	out := events.ContainerSetup{ID: ct.ID, Name: p.Namespace + "/" + p.Name + "/" + ct.Name, Image: ct.Image,
		Privileged: ct.Privileged, HostPID: p.HostPID, HostNetwork: p.HostNetwork, HostIPC: p.HostIPC,
		CapAdd: ct.CapAdd, RunsAsRoot: ct.RunsAsRoot, ReadOnlyRoot: ct.ReadOnlyRoot}
	for _, m := range ct.HostMounts {
		out.Mounts = append(out.Mounts, events.HostMount{Path: m.Path, Writable: m.Writable})
	}
	return out
}
