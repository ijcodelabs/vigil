//go:build linux

package events

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"

	"github.com/ijcodelabs/vigil/internal/config"
)

// The probes every kernel gets, and the two ways of seeing opens and
// privilege changes (bpf/events.c): fentry where BPF trampolines work,
// otherwise the system-call probes. Only one way is loaded.
type probeMaps struct {
	Events  *ebpf.Map      `ebpf:"events"`
	Watch   *ebpf.Map      `ebpf:"watch"`
	Scratch *ebpf.Map      `ebpf:"scratch"`
	Exited  *ebpf.Map      `ebpf:"exited"`
	Lost    *ebpf.Variable `ebpf:"lost"`
	OnExec  *ebpf.Program  `ebpf:"on_exec"`
	OnExit  *ebpf.Program  `ebpf:"on_exit"`
	OnSock  *ebpf.Program  `ebpf:"on_sock_state"`
}

type fentryProbes struct {
	probeMaps
	OnFileOpen    *ebpf.Program `ebpf:"on_file_open"`
	OnOpenRefused *ebpf.Program `ebpf:"on_open_refused"`
	OnCommitCreds *ebpf.Program `ebpf:"on_commit_creds"`
}

type syscallProbes struct {
	probeMaps
	CredBefore *ebpf.Map     `ebpf:"cred_before"`
	OnSysEnter *ebpf.Program `ebpf:"on_sys_enter"`
	OnSysExit  *ebpf.Program `ebpf:"on_sys_exit"`
}

type kernelSource struct {
	maps   *probeMaps
	closer []io.Closer
	links  []link.Link
	rd     *ringbuf.Reader
	boot   time.Time
	// Mode is "fentry" or "syscalls", for the log and the agent's status.
	mode string
}

// openSource loads the probes into the kernel and attaches them.
func openSource(cfg *config.Security, log *slog.Logger) (source, error) {
	if _, err := os.Stat("/sys/kernel/btf/vmlinux"); err != nil {
		return nil, fmt.Errorf("%w: this kernel has no BTF type information (/sys/kernel/btf/vmlinux)", ErrUnsupported)
	}
	rlimit.RemoveMemlock() // only matters before Linux 5.11

	spec, err := loadEvents()
	if err != nil {
		return nil, err
	}
	s := &kernelSource{}
	fail := func(err error) (source, error) { s.Close(); return nil, err }
	attach := func(progs map[string]*ebpf.Program) error {
		// Every probe attaches by kernel type information alone: no tracefs.
		for name, prog := range progs {
			l, err := link.AttachTracing(link.TracingOptions{Program: prog})
			if err != nil {
				return fmt.Errorf("attaching the %s probe: %v", name, err)
			}
			s.links = append(s.links, l)
		}
		return nil
	}
	base := func(m *probeMaps) map[string]*ebpf.Program {
		return map[string]*ebpf.Program{"process start": m.OnExec, "process end": m.OnExit, "network": m.OnSock}
	}

	// fentry first; a kernel that cannot load or attach it gets the
	// system-call probes, with the reason in the log.
	// VIGIL_EVENTS_PROBES=syscalls skips fentry: a way out should a kernel
	// misbehave with it, and how the fallback is tested.
	var fe fentryProbes
	if os.Getenv("VIGIL_EVENTS_PROBES") == "syscalls" {
		err = errors.New("VIGIL_EVENTS_PROBES=syscalls")
	} else {
		err = spec.LoadAndAssign(&fe, nil)
	}
	if err == nil {
		s.maps, s.closer, s.mode = &fe.probeMaps, []io.Closer{fe.Events, fe.Watch, fe.Scratch, fe.Exited,
			fe.OnExec, fe.OnExit, fe.OnSock, fe.OnFileOpen, fe.OnOpenRefused, fe.OnCommitCreds}, "fentry"
		progs := base(&fe.probeMaps)
		progs["file open"], progs["refused open"], progs["privilege change"] = fe.OnFileOpen, fe.OnOpenRefused, fe.OnCommitCreds
		if err = attach(progs); err != nil {
			s.Close()
			s = &kernelSource{}
		}
	}
	if err != nil {
		if errors.Is(err, unix.EPERM) || errors.Is(err, unix.EACCES) {
			return nil, fmt.Errorf("%w: not permitted to load eBPF programs (run as root, or grant CAP_BPF and CAP_PERFMON)", ErrUnsupported)
		}
		log.Info("runtime events: fentry probes unavailable, using the system-call probes (opens by relative path are not seen)", "reason", firstLine(err))
		var sc syscallProbes
		if err := spec.LoadAndAssign(&sc, nil); err != nil {
			var ve *ebpf.VerifierError
			switch {
			case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
				return nil, fmt.Errorf("%w: not permitted to load eBPF programs (run as root, or grant CAP_BPF and CAP_PERFMON)", ErrUnsupported)
			case errors.As(err, &ve):
				return nil, fmt.Errorf("%w: the kernel rejected the probe: %v", ErrUnsupported, err)
			}
			return nil, fmt.Errorf("%w: loading the probe failed (Linux 5.8 or newer is needed): %v", ErrUnsupported, err)
		}
		s.maps, s.closer, s.mode = &sc.probeMaps, []io.Closer{sc.Events, sc.Watch, sc.Scratch, sc.Exited, sc.CredBefore,
			sc.OnExec, sc.OnExit, sc.OnSock, sc.OnSysEnter, sc.OnSysExit}, "syscalls"
		progs := base(&sc.probeMaps)
		progs["system call entry"], progs["system call exit"] = sc.OnSysEnter, sc.OnSysExit
		if err := attach(progs); err != nil {
			return fail(fmt.Errorf("%w: %v", ErrUnsupported, err))
		}
	}

	for i, p := range cfg.WatchPaths {
		v := eventsPrefix{Len: uint32(len(p))}
		for j := 0; j < len(p) && j < len(v.S); j++ {
			v.S[j] = int8(p[j])
		}
		if err := s.maps.Watch.Put(uint32(i), &v); err != nil {
			return fail(fmt.Errorf("setting watch paths: %w", err))
		}
	}

	rd, err := ringbuf.NewReader(s.maps.Events)
	if err != nil {
		return fail(err)
	}
	s.rd = rd

	// Kernel timestamps count from boot; find the wall-clock time of zero.
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return fail(err)
	}
	s.boot = time.Now().Add(-time.Duration(ts.Nano()))
	log.Info("runtime events: probes loaded", "mode", s.mode)
	return s, nil
}

func firstLine(err error) string {
	msg := err.Error()
	if i := strings.IndexByte(msg, '\n'); i >= 0 {
		msg = msg[:i]
	}
	return msg
}

func (s *kernelSource) Read() ([]byte, error) {
	rec, err := s.rd.Read()
	if err != nil {
		return nil, err
	}
	return rec.RawSample, nil
}

func (s *kernelSource) BootTime() time.Time { return s.boot }

func (s *kernelSource) Lost() uint64 {
	var n uint64
	if s.maps != nil && s.maps.Lost != nil {
		s.maps.Lost.Get(&n)
	}
	return n
}

func (s *kernelSource) Close() error {
	if s.rd != nil {
		s.rd.Close()
	}
	for _, l := range s.links {
		l.Close()
	}
	for _, c := range s.closer {
		if c != nil && !isNilCloser(c) {
			c.Close()
		}
	}
	return nil
}

// isNilCloser: a nil *ebpf.Map or *ebpf.Program in an io.Closer is not nil.
func isNilCloser(c io.Closer) bool {
	switch v := c.(type) {
	case *ebpf.Map:
		return v == nil
	case *ebpf.Program:
		return v == nil
	}
	return false
}
