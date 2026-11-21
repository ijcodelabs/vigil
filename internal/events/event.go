// Package events is the agent's runtime-event pipeline: kernel probes
// report what processes do, rules are evaluated on each event right on the
// host, and a flight recorder keeps the recent past so a capture can show
// what led up to a detection.
package events

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net/netip"
	"strings"
	"time"
)

const (
	TypeExec    = "exec"    // a program was executed
	TypeExit    = "exit"    // a process ended
	TypeConnect = "connect" // outbound TCP connection attempt
	TypeListen  = "listen"  // a TCP socket started listening
	TypeOpen    = "open"    // a file under a watched path was opened
	TypeSetuid  = "setuid"  // the effective user id changed
	// TypeContainer is not from the kernel: a container the agent saw start,
	// with how it was started.
	TypeContainer = "container"
	// What a process needs to reach the host from a container.
	TypeNamespace = "namespace" // joined (setns) or made (unshare) namespaces
	TypeMount     = "mount"     // mounted something
	TypePtrace    = "ptrace"    // attached to another process
	TypeModule    = "module"    // a kernel module was loaded
	TypeBPF       = "bpf"       // a BPF program is being loaded
)

// Record types of bpf/events.c. 10 is a module or a BPF program (a field
// says which); 11 is a unix-socket connect, reported as TypeConnect.
var typeNames = map[uint32]string{1: TypeExec, 2: TypeExit, 3: TypeConnect, 4: TypeListen, 5: TypeOpen, 6: TypeSetuid,
	7: TypeNamespace, 8: TypeMount, 9: TypePtrace, 10: TypeModule, 11: TypeConnect}

// nsNames are the NS_* bits of bpf/events.c.
var nsNames = []string{"mnt", "uts", "ipc", "pid", "net", "cgroup"}

const (
	msRemount = 32   // MS_REMOUNT
	msBind    = 4096 // MS_BIND
)

// Event is one thing a process did. Which fields are set depends on Type.
type Event struct {
	Time time.Time `json:"time"`
	Type string    `json:"type"`
	PID  uint32    `json:"pid"`
	PPID uint32    `json:"ppid"`
	UID  uint32    `json:"uid"`
	EUID uint32    `json:"euid"`
	Comm string    `json:"comm"`
	// Exe is the program file: for exec the one being started, otherwise
	// the one the process runs.
	Exe         string   `json:"exe,omitempty"`
	Args        []string `json:"args,omitempty"` // exec
	ParentComm  string   `json:"parent_comm,omitempty"`
	ParentExe   string   `json:"parent_exe,omitempty"`
	Container   string   `json:"container,omitempty"` // name, or short id
	ContainerID string   `json:"container_id,omitempty"`

	Path     string  `json:"path,omitempty"`      // open
	Write    bool    `json:"write,omitempty"`     // open: for writing or creating
	Refused  bool    `json:"refused,omitempty"`   // open: denied by the kernel (no permission)
	Addr     string  `json:"addr,omitempty"`      // connect: remote; listen: local
	Port     uint16  `json:"port,omitempty"`      // connect: remote; listen: local
	Unix     bool    `json:"unix,omitempty"`      // connect: to a container runtime's unix socket, Addr is its path
	OldEUID  *uint32 `json:"old_euid,omitempty"`  // setuid
	NewEUID  *uint32 `json:"new_euid,omitempty"`  // setuid
	ExitCode *int32  `json:"exit_code,omitempty"` // exit
	Signal   int32   `json:"signal,omitempty"`    // exit: killed by this signal

	// container: how it was started.
	Image         string   `json:"image,omitempty"`
	Privileged    bool     `json:"privileged,omitempty"`
	HostPID       bool     `json:"host_pid,omitempty"`
	HostNetwork   bool     `json:"host_network,omitempty"`
	HostIPC       bool     `json:"host_ipc,omitempty"`
	CapAdd        []string `json:"cap_add,omitempty"`
	HostMounts    []string `json:"host_mounts,omitempty"`    // host paths mounted in
	HostMountsRW  []string `json:"host_mounts_rw,omitempty"` // the writable ones
	RuntimeSocket string   `json:"runtime_socket,omitempty"` // a runtime's API socket, or its directory, mounted in
	RunsAsRoot    bool     `json:"runs_as_root,omitempty"`
	ReadOnlyRoot  bool     `json:"read_only_root,omitempty"`

	// namespace: the ones that changed, and those of them now the host's.
	Namespaces     []string `json:"namespaces,omitempty"`
	HostNamespaces []string `json:"host_namespaces,omitempty"`
	// mount: Target is the mount point's last two components.
	Source     string `json:"source,omitempty"`
	FSType     string `json:"fstype,omitempty"`
	Target     string `json:"target,omitempty"`
	MountFlags uint64 `json:"mount_flags,omitempty"`
	// ptrace
	TargetPID  uint32 `json:"target_pid,omitempty"`
	TargetComm string `json:"target_comm,omitempty"`
	// module: the kernel module's name; bpf: the program's name and type.
	Module   string `json:"module,omitempty"`
	ProgName string `json:"prog_name,omitempty"`
	ProgType uint32 `json:"prog_type,omitempty"`

	cgroup uint64
}

// Sizes and offsets of the records written by bpf/events.c.
const (
	hdrSize    = 56
	execSize   = hdrSize + 8 + 256 + 1024
	exitSize   = hdrSize + 8
	netSize    = hdrSize + 8 + 32
	openSize   = hdrSize + 8 + 256
	credSize   = hdrSize + 8
	nsSize     = hdrSize + 8
	mountSize  = hdrSize + 8 + 128 + 32 + 64 + 64
	ptraceSize = hdrSize + 8 + 16
	moduleSize = hdrSize + 8 + 64
	unixSize   = hdrSize + 128

	oWriteMask = 0x1 | 0x2 | 0x40 | 0x200 // O_WRONLY, O_RDWR, O_CREAT, O_TRUNC
	afInet     = 2
)

var errShort = errors.New("events: record too short")

func cstr(b []byte) string {
	if i := bytes.IndexByte(b, 0); i >= 0 {
		b = b[:i]
	}
	return string(b)
}

// decode turns a ring-buffer record into an Event. bootTime is the wall
// clock at kernel time zero.
func decode(raw []byte, bootTime time.Time) (*Event, error) {
	if len(raw) < hdrSize {
		return nil, errShort
	}
	le := binary.LittleEndian
	rec := le.Uint32(raw[16:])
	ev := &Event{
		Time:   bootTime.Add(time.Duration(le.Uint64(raw[0:]))),
		cgroup: le.Uint64(raw[8:]),
		Type:   typeNames[rec],
		PID:    le.Uint32(raw[20:]),
		PPID:   le.Uint32(raw[28:]),
		UID:    le.Uint32(raw[32:]),
		EUID:   le.Uint32(raw[36:]),
		Comm:   cstr(raw[40:56]),
	}
	need := map[uint32]int{1: execSize, 2: exitSize, 3: netSize, 4: netSize, 5: openSize, 6: credSize,
		7: nsSize, 8: mountSize, 9: ptraceSize, 10: moduleSize, 11: unixSize}[rec]
	if need == 0 {
		return nil, errors.New("events: unknown record type")
	}
	if len(raw) < need {
		return nil, errShort
	}
	p := raw[hdrSize:]
	switch rec {
	case 7:
		changed, host := le.Uint32(p), le.Uint32(p[4:])
		for i, name := range nsNames {
			if changed&(1<<i) != 0 {
				ev.Namespaces = append(ev.Namespaces, name)
			}
			if host&(1<<i) != 0 {
				ev.HostNamespaces = append(ev.HostNamespaces, name)
			}
		}
		return ev, nil
	case 8:
		ev.MountFlags = le.Uint64(p)
		ev.Source, ev.FSType = cstr(p[8:136]), cstr(p[136:168])
		parent, name := cstr(p[168:232]), cstr(p[232:296])
		switch {
		case parent == "" || parent == "/":
			ev.Target = "/" + strings.TrimPrefix(name, "/")
		default:
			ev.Target = ".../" + parent + "/" + name
		}
		return ev, nil
	case 9:
		ev.TargetPID, ev.TargetComm = le.Uint32(p), cstr(p[8:24])
		return ev, nil
	case 10:
		if le.Uint32(p) == 2 {
			ev.Type, ev.ProgType, ev.ProgName = TypeBPF, le.Uint32(p[4:]), cstr(p[8:72])
		} else {
			ev.Module = cstr(p[8:72])
		}
		return ev, nil
	case 11:
		ev.Unix, ev.Addr = true, cstr(p[:128])
		return ev, nil
	}
	switch ev.Type {
	case TypeExec:
		n := int(le.Uint32(p))
		ev.Exe = cstr(p[8 : 8+256])
		if args := p[264:]; n > 0 && n <= len(args) {
			for _, a := range bytes.Split(bytes.TrimRight(args[:n], "\x00"), []byte{0}) {
				ev.Args = append(ev.Args, string(a))
			}
		}
	case TypeExit:
		status := int32(le.Uint32(p))
		code := status >> 8 & 0xff
		ev.ExitCode, ev.Signal = &code, status&0x7f
	case TypeConnect, TypeListen:
		family, sport, dport := le.Uint16(p), le.Uint16(p[2:]), le.Uint16(p[4:])
		addr := func(b []byte) string {
			if family == afInet {
				return netip.AddrFrom4([4]byte(b[:4])).String()
			}
			return netip.AddrFrom16([16]byte(b[:16])).Unmap().String()
		}
		if ev.Type == TypeConnect {
			ev.Addr, ev.Port = addr(p[24:40]), dport
		} else {
			ev.Addr, ev.Port = addr(p[8:24]), sport
		}
	case TypeOpen:
		ev.Write = int32(le.Uint32(p))&oWriteMask != 0
		ev.Refused = le.Uint32(p[4:]) != 0
		ev.Path = cstr(p[8 : 8+256])
	case TypeSetuid:
		old, now := le.Uint32(p), le.Uint32(p[4:])
		ev.OldEUID, ev.NewEUID = &old, &now
	}
	return ev, nil
}
