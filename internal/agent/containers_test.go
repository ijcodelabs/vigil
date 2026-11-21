package agent

import (
	"testing"

	"github.com/ijcodelabs/vigil/internal/dockerapi"
	"github.com/ijcodelabs/vigil/internal/kube"
)

func TestContainerSetups(t *testing.T) {
	for user, root := range map[string]bool{"": true, "0": true, "root": true, "0:0": true, "root:wheel": true,
		"1000": false, "app": false, "1000:0": false} {
		if isRootUser(user) != root {
			t.Errorf("isRootUser(%q) = %v", user, !root)
		}
	}
	d := dockerSetup("abc", "shop-api", dockerapi.Setup{Image: "acme/api:1", User: "app", PidMode: "host",
		NetworkMode: "bridge", IpcMode: "host", Binds: []dockerapi.Bind{{Source: "/etc", RW: false}}})
	if !d.HostPID || d.HostNetwork || !d.HostIPC || d.RunsAsRoot || len(d.Mounts) != 1 || d.Mounts[0].Writable || d.Name != "shop-api" {
		t.Errorf("docker setup: %+v", d)
	}
	k := podSetup(kube.Pod{Name: "api-1", Namespace: "prod", HostNetwork: true},
		kube.Container{Name: "api", ID: "k1", Privileged: true, HostMounts: []kube.HostMount{{Path: "/var/run", Writable: true}}})
	if k.Name != "prod/api-1/api" || !k.HostNetwork || !k.Privileged || len(k.Mounts) != 1 || !k.Mounts[0].Writable {
		t.Errorf("pod setup: %+v", k)
	}
}
