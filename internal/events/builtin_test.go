package events

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ijcodelabs/vigil/internal/config"
)

func pack(t *testing.T, mod func(*config.Security)) (*Engine, *[]string) {
	t.Helper()
	c, err := config.ParseCollection("")
	if err != nil {
		t.Fatal(err)
	}
	if mod != nil {
		mod(&c.Security)
	}
	e, err := NewEngine(&c.Security)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	e.OnDetection = func(d Detection) { got = append(got, d.Rule) }
	return e, &got
}

func TestBuiltinPackShape(t *testing.T) {
	names := map[string]bool{}
	for _, r := range builtinRules() {
		if names[r.Name] {
			t.Errorf("%s twice", r.Name)
		}
		names[r.Name] = true
		if r.Severity == "" || r.Summary == "" || r.Capture {
			t.Errorf("%s: every built-in rule has a severity and a summary and captures nothing by default", r.Name)
		}
	}
	if len(names) < 15 {
		t.Errorf("only %d built-in rules", len(names))
	}
}

// Each built-in rule fires on its case and stays quiet on a near miss.
func TestBuiltinRules(t *testing.T) {
	ct := func(s ContainerSetup) *Event {
		s.ID, s.Image = strings.Repeat("a", 64), "acme/api:1"
		return s.event(time.Now())
	}
	open := func(comm, path string, write bool, container string) *Event {
		return &Event{Type: TypeOpen, Comm: comm, Path: path, Write: write, Container: container}
	}
	exec := func(comm, exe, parent, container string) *Event {
		return &Event{Type: TypeExec, Comm: comm, Exe: exe, ParentComm: parent, Container: container, Args: []string{exe}}
	}
	u := func(v uint32) *uint32 { return &v }
	for _, c := range []struct {
		rule      string
		fire, not *Event
	}{
		{"privileged_container", ct(ContainerSetup{Privileged: true}), ct(ContainerSetup{})},
		{"host_namespace_container", ct(ContainerSetup{HostPID: true}), ct(ContainerSetup{HostNetwork: true})},
		{"runtime_socket_in_container", ct(ContainerSetup{Mounts: []HostMount{{Path: "/var/run/docker.sock"}}}),
			ct(ContainerSetup{Mounts: []HostMount{{Path: "/srv/data"}}})},
		{"dangerous_capability", ct(ContainerSetup{CapAdd: []string{"CAP_SYS_PTRACE"}}), ct(ContainerSetup{CapAdd: []string{"NET_BIND_SERVICE"}})},
		{"sensitive_host_mount", ct(ContainerSetup{Mounts: []HostMount{{Path: "/etc"}}}),
			ct(ContainerSetup{Mounts: []HostMount{{Path: "/etc/localtime"}}})},
		{"sensitive_host_mount_writable", ct(ContainerSetup{Mounts: []HostMount{{Path: "/", Writable: true}}}),
			ct(ContainerSetup{Mounts: []HostMount{{Path: "/"}}})},
		{"system_config_changed", open("vim", "/root/.ssh/authorized_keys", true, ""), open("dpkg", "/etc/ssh/sshd_config", true, "")},
		{"scheduled_task_changed", open("bash", "/etc/cron.d/job", true, ""), open("cron", "/etc/cron.d/job", false, "")},
		{"password_file_changed", open("python3", "/etc/shadow", true, ""), open("passwd", "/etc/shadow", true, "")},
		{"user_account_changed", exec("useradd", "/usr/sbin/useradd", "bash", ""), exec("useradd", "/usr/sbin/useradd", "sh", "web")},
		{"sensitive_file_read", open("cat", "/etc/shadow", false, ""), open("pg_isready", "/etc/shadow", false, "db")},
		{"sensitive_file_read", open("python3", "/etc/shadow", false, ""),
			&Event{Type: TypeOpen, Comm: "python3", Path: "/etc/shadow", ParentComm: "dpkg"}},
		{"became_root", &Event{Type: TypeSetuid, Comm: "exploit", UID: 1000, OldEUID: u(1000), NewEUID: u(0)},
			&Event{Type: TypeSetuid, Comm: "sudo", UID: 1000, OldEUID: u(1000), NewEUID: u(0)}},
		{"became_root", &Event{Type: TypeSetuid, Comm: "www", UID: 33, OldEUID: u(33), NewEUID: u(0)}, // a service account
			&Event{Type: TypeSetuid, Comm: "gdbus", UID: 0, OldEUID: u(42), NewEUID: u(0)}}, // apt taking root back
		{"became_root", &Event{Type: TypeSetuid, Comm: "(fwupdmgr)", ParentComm: "bash", UID: 1000, OldEUID: u(1000), NewEUID: u(0)},
			&Event{Type: TypeSetuid, Comm: "(fwupdmgr)", ParentComm: "systemd", UID: 1000, OldEUID: u(1000), NewEUID: u(0)}},
		{"run_from_temp_dir", exec("x", "/tmp/x", "bash", ""), exec("ls", "/usr/bin/ls", "bash", "")},
		{"unexpected_listener", &Event{Type: TypeListen, Comm: "nc", Port: 23}, &Event{Type: TypeListen, Comm: "nginx", Port: 80}},
		{"shell_in_container", exec("sh", "/bin/sh", "node", "web"), exec("sh", "/bin/sh", "runc", "web")},
		{"download_tool_in_container", exec("curl", "/usr/bin/curl", "sh", "web"), exec("curl", "/usr/bin/curl", "sh", "")},
		{"package_manager_in_container", exec("apk", "/sbin/apk", "sh", "web"), exec("apk", "/sbin/apk", "sh", "")},
	} {
		e, got := pack(t, nil)
		c.fire.Time, c.not.Time = time.Now(), time.Now()
		e.Handle(c.fire)
		if !slices.Contains(*got, c.rule) {
			t.Errorf("%s did not fire on %+v (fired: %v)", c.rule, c.fire, *got)
		}
		*got = nil
		e.Handle(c.not)
		if slices.Contains(*got, c.rule) {
			t.Errorf("%s fired on the near miss %+v", c.rule, c.not)
		}
	}
	// The agent's own container needs what it has.
	e, got := pack(t, nil)
	e.Handle(ContainerSetup{ID: strings.Repeat("b", 64), Image: "ghcr.io/ijcodelabs/vigil-agent:0.3.0", Privileged: true,
		HostPID: true, Mounts: []HostMount{{Path: "/", Writable: false}, {Path: "/var/run/docker.sock"}}}.event(time.Now()))
	if len(*got) != 0 {
		t.Errorf("the vigil agent's own container fired %v", *got)
	}
}

func TestBuiltinOverrides(t *testing.T) {
	no := false
	priv := ContainerSetup{ID: strings.Repeat("c", 64), Privileged: true}

	e, got := pack(t, func(s *config.Security) { s.DisableRules = []string{"privileged_container"} })
	e.Handle(priv.event(time.Now()))
	if len(*got) != 0 {
		t.Errorf("disabled rule fired: %v", *got)
	}
	e, got = pack(t, func(s *config.Security) {
		s.Rules = []config.SecurityRule{{Name: "privileged_container", When: `type == "container" && privileged && container == "other"`}}
	})
	e.Handle(priv.event(time.Now()))
	if len(*got) != 0 {
		t.Errorf("a configured rule of the same name did not replace the built-in one: %v", *got)
	}
	e, got = pack(t, func(s *config.Security) {
		s.Rules = []config.SecurityRule{{Name: "privileged_container", Enabled: &no}}
	})
	e.Handle(priv.event(time.Now()))
	if len(*got) != 0 {
		t.Errorf("enabled: false on a built-in name did not turn it off: %v", *got)
	}
	e, got = pack(t, func(s *config.Security) { s.BuiltinRules = &no })
	e.Handle(priv.event(time.Now()))
	if len(*got) != 0 || len(e.rules) != 0 {
		t.Errorf("builtin_rules: false left %d rules", len(e.rules))
	}

	c, _ := config.ParseCollection("")
	c.Security.DisableRules = []string{"no_such_rule"}
	if err := Validate(&c.Security); err == nil || !strings.Contains(err.Error(), "not a built-in rule") {
		t.Errorf("unknown disable_rules name: %v", err)
	}
}

func TestWatchPaths(t *testing.T) {
	cfg := &config.Security{WatchPaths: []string{"/etc/", "/srv/app/"}}
	paths, skipped := watchPaths(cfg)
	if !slices.Equal(paths, []string{"/etc/", "/srv/app/", "/root/.ssh/", "/var/spool/cron/"}) || skipped != nil {
		t.Errorf("paths %v skipped %v: built-in paths under a configured /etc/ are covered", paths, skipped)
	}
	cfg.WatchPaths = nil
	for i := range 14 {
		cfg.WatchPaths = append(cfg.WatchPaths, "/data/"+string(rune('a'+i)))
	}
	paths, skipped = watchPaths(cfg)
	if len(paths) != maxWatchPaths || len(skipped) != len(builtinWatchPaths)-2 {
		t.Errorf("full list: %d paths, skipped %v", len(paths), skipped)
	}
	no := false
	cfg.BuiltinRules = &no
	if paths, _ := watchPaths(cfg); len(paths) != 14 {
		t.Errorf("builtin_rules: false still adds paths: %v", paths)
	}
}
