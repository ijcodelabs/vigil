//go:build linux

package events

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/ringbuf"
	"github.com/cilium/ebpf/rlimit"
	"golang.org/x/sys/unix"

	"github.com/ijcodelabs/vigil/internal/config"
)

type kernelSource struct {
	objs  eventsObjects
	links []link.Link
	rd    *ringbuf.Reader
	boot  time.Time
}

// openSource loads the probes into the kernel and attaches them.
func openSource(cfg *config.Security, _ *slog.Logger) (source, error) {
	if _, err := os.Stat("/sys/kernel/btf/vmlinux"); err != nil {
		return nil, fmt.Errorf("%w: this kernel has no BTF type information (/sys/kernel/btf/vmlinux)", ErrUnsupported)
	}
	rlimit.RemoveMemlock() // only matters before Linux 5.11

	s := &kernelSource{}
	if err := loadEventsObjects(&s.objs, nil); err != nil {
		var ve *ebpf.VerifierError
		switch {
		case errors.Is(err, unix.EPERM), errors.Is(err, unix.EACCES):
			return nil, fmt.Errorf("%w: not permitted to load eBPF programs (run as root, or grant CAP_BPF and CAP_PERFMON)", ErrUnsupported)
		case errors.As(err, &ve):
			return nil, fmt.Errorf("%w: the kernel rejected the probe: %v", ErrUnsupported, err)
		}
		return nil, fmt.Errorf("%w: loading the probe failed (Linux 5.8 or newer is needed): %v", ErrUnsupported, err)
	}
	fail := func(err error) (source, error) { s.Close(); return nil, err }

	for i, p := range cfg.WatchPaths {
		v := eventsPrefix{Len: uint32(len(p))}
		for j := 0; j < len(p) && j < len(v.S); j++ {
			v.S[j] = int8(p[j])
		}
		if err := s.objs.Watch.Put(uint32(i), &v); err != nil {
			return fail(fmt.Errorf("setting watch paths: %w", err))
		}
	}

	// Every probe attaches by kernel type information alone: no tracefs.
	for name, prog := range map[string]*ebpf.Program{"process start": s.objs.OnExec, "process end": s.objs.OnExit,
		"network": s.objs.OnSockState, "system call entry": s.objs.OnSysEnter, "system call exit": s.objs.OnSysExit} {
		l, err := link.AttachTracing(link.TracingOptions{Program: prog})
		if err != nil {
			return fail(fmt.Errorf("%w: attaching the %s probe: %v", ErrUnsupported, name, err))
		}
		s.links = append(s.links, l)
	}

	rd, err := ringbuf.NewReader(s.objs.Events)
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
	return s, nil
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
	if s.objs.Lost != nil {
		s.objs.Lost.Get(&n)
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
	return s.objs.Close()
}
