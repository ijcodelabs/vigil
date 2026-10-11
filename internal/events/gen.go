package events

// The compiled probes (events_x86_bpfel.o, events_arm64_bpfel.o) and their Go
// bindings are generated from bpf/events.c and committed, so building vigil needs no clang. Regenerate
// with `make bpf` (runs this in a container that has clang and libbpf).
//go:generate go run github.com/cilium/ebpf/cmd/bpf2go -cc clang -cflags "-O2 -g -Wall -Werror" -target amd64,arm64 -go-package events events bpf/events.c
