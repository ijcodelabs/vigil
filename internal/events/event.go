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
	"time"
)

const (
	TypeExec    = "exec"    // a program was executed
	TypeExit    = "exit"    // a process ended
	TypeConnect = "connect" // outbound TCP connection attempt
	TypeListen  = "listen"  // a TCP socket started listening
	TypeOpen    = "open"    // a file under a watched path was opened
	TypeSetuid  = "setuid"  // the effective user id changed
)

var typeNames = map[uint32]string{1: TypeExec, 2: TypeExit, 3: TypeConnect, 4: TypeListen, 5: TypeOpen, 6: TypeSetuid}

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
	Addr     string  `json:"addr,omitempty"`      // connect: remote; listen: local
	Port     uint16  `json:"port,omitempty"`      // connect: remote; listen: local
	OldEUID  *uint32 `json:"old_euid,omitempty"`  // setuid
	NewEUID  *uint32 `json:"new_euid,omitempty"`  // setuid
	ExitCode *int32  `json:"exit_code,omitempty"` // exit
	Signal   int32   `json:"signal,omitempty"`    // exit: killed by this signal

	cgroup uint64
}

// Sizes and offsets of the records written by bpf/events.c.
const (
	hdrSize  = 56
	execSize = hdrSize + 8 + 256 + 1024
	exitSize = hdrSize + 8
	netSize  = hdrSize + 8 + 32
	openSize = hdrSize + 8 + 256
	credSize = hdrSize + 8

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
	ev := &Event{
		Time:   bootTime.Add(time.Duration(le.Uint64(raw[0:]))),
		cgroup: le.Uint64(raw[8:]),
		Type:   typeNames[le.Uint32(raw[16:])],
		PID:    le.Uint32(raw[20:]),
		PPID:   le.Uint32(raw[28:]),
		UID:    le.Uint32(raw[32:]),
		EUID:   le.Uint32(raw[36:]),
		Comm:   cstr(raw[40:56]),
	}
	need := map[string]int{TypeExec: execSize, TypeExit: exitSize, TypeConnect: netSize, TypeListen: netSize,
		TypeOpen: openSize, TypeSetuid: credSize}[ev.Type]
	if need == 0 {
		return nil, errors.New("events: unknown record type")
	}
	if len(raw) < need {
		return nil, errShort
	}
	p := raw[hdrSize:]
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
		ev.Path = cstr(p[8 : 8+256])
	case TypeSetuid:
		old, now := le.Uint32(p), le.Uint32(p[4:])
		ev.OldEUID, ev.NewEUID = &old, &now
	}
	return ev, nil
}
