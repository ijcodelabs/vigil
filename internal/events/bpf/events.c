// Kernel side of vigil's runtime events. Each probe writes one fixed-layout
// record to a ring buffer; internal/events/decode.go reads them.
//
// The file is self-contained on purpose: the few kernel structs it touches
// are declared here with only the fields it reads, and CO-RE relocates them
// against the running kernel's BTF. No vmlinux.h, no kernel headers.
//
// Needs: BTF (CONFIG_DEBUG_INFO_BTF) and the BPF ring buffer (Linux 5.8).
// Rebuild with `make bpf` after changing this file. It is compiled once per
// CPU architecture, because system-call numbers and the registers holding
// their arguments differ.

typedef unsigned char __u8;
typedef unsigned short __u16;
typedef unsigned int __u32;
typedef unsigned long long __u64;
typedef signed char __s8;
typedef short __s16;
typedef int __s32;
typedef long long __s64;
typedef __u16 __be16;
typedef __u32 __be32;
typedef __u32 __wsum;
typedef _Bool bool;
#define true 1
#define false 0

#define BPF_MAP_TYPE_HASH 1
#define BPF_MAP_TYPE_ARRAY 2
#define BPF_MAP_TYPE_PERCPU_ARRAY 6
#define BPF_MAP_TYPE_RINGBUF 27
#define BPF_ANY 0

// Tells libbpf's headers that kernel-style register names (di, si, ...) are
// in use, as they would be with a generated vmlinux.h.
#define __VMLINUX_H__

#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_endian.h>

#pragma clang attribute push(__attribute__((preserve_access_index)), apply_to = record)
typedef struct { __u32 val; } kuid_t;
struct cred { kuid_t uid; kuid_t euid; };
struct mm_struct { unsigned long arg_start; unsigned long arg_end; };
struct task_struct {
	int pid;
	int tgid;
	struct task_struct *real_parent;
	struct mm_struct *mm;
	const struct cred *cred;
	int exit_code;
};
struct linux_binprm { const char *filename; };
struct in6_addr { union { __u8 u6_addr8[16]; } in6_u; };
struct sock_common {
	union { struct { __be32 skc_daddr; __be32 skc_rcv_saddr; }; };
	union { struct { __be16 skc_dport; __u16 skc_num; }; };
	unsigned short skc_family;
	struct in6_addr skc_v6_daddr;
	struct in6_addr skc_v6_rcv_saddr;
};
struct sock { struct sock_common __sk_common; };
// The registers a system call's arguments arrive in.
#if defined(__TARGET_ARCH_x86)
struct pt_regs { unsigned long di; unsigned long si; unsigned long dx; unsigned long r10; };
#elif defined(__TARGET_ARCH_arm64)
struct user_pt_regs { __u64 regs[31]; };
struct pt_regs { struct user_pt_regs user_regs; };
#endif
#pragma clang attribute pop

#if defined(__TARGET_ARCH_x86)
#define NR_open 2
#define NR_openat 257
#define NR_openat2 437
#define NR_setuid 105
#define NR_setreuid 113
#define NR_setresuid 117
#elif defined(__TARGET_ARCH_arm64)
#define NR_openat 56
#define NR_openat2 437
#define NR_setuid 146
#define NR_setreuid 145
#define NR_setresuid 147
#else
#error "unsupported architecture: build with -target amd64 or arm64"
#endif

#define COMM_LEN 16
#define PATH_LEN 256
#define ARGS_LEN 1024
#define MAX_PREFIXES 16
#define PREFIX_LEN 64

#define AF_INET 2
#define AF_INET6 10
#define TCP_SYN_SENT 2
#define TCP_CLOSE 7
#define TCP_LISTEN 10

enum { EV_EXEC = 1, EV_EXIT = 2, EV_CONNECT = 3, EV_LISTEN = 4, EV_OPEN = 5, EV_SETUID = 6 };

// Every record starts with this header (56 bytes).
struct hdr {
	__u64 ts; // nanoseconds, CLOCK_MONOTONIC
	__u64 cgroup;
	__u32 type;
	__u32 pid; // process (thread group) id
	__u32 tid;
	__u32 ppid;
	__u32 uid;
	__u32 euid;
	char comm[COMM_LEN];
};
struct exec_event { struct hdr h; __u32 args_len; __u32 pad; char filename[PATH_LEN]; char args[ARGS_LEN]; };
struct exit_event { struct hdr h; __s32 code; __u32 pad; };
struct net_event { struct hdr h; __u16 family; __u16 sport; __u16 dport; __u16 pad; __u8 saddr[16]; __u8 daddr[16]; };
struct open_event { struct hdr h; __s32 flags; __u32 pad; char path[PATH_LEN]; };
struct cred_event { struct hdr h; __u32 old_euid; __u32 new_euid; };

struct { __uint(type, BPF_MAP_TYPE_RINGBUF); __uint(max_entries, 8 << 20); } events SEC(".maps");

// Paths whose opens are reported, packed from index 0; len 0 ends the list.
struct prefix { __u32 len; char s[PREFIX_LEN]; };
struct { __uint(type, BPF_MAP_TYPE_ARRAY); __uint(max_entries, MAX_PREFIXES); __type(key, __u32); __type(value, struct prefix); } watch SEC(".maps");

// An open record is built here first, so opens that match no prefix never
// touch the ring buffer.
struct { __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY); __uint(max_entries, 1); __type(key, __u32); __type(value, struct open_event); } scratch SEC(".maps");

// Effective uid of a thread on entry to a set*uid call.
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 4096); __type(key, __u64); __type(value, __u32); } cred_before SEC(".maps");

// Records dropped because the ring buffer was full.
__u64 lost = 0;

// Threads currently inside a set*uid call. While it is zero, the
// system-call exit probe returns at once.
__u64 in_setuid = 0;

static __always_inline __u32 current_euid(void)
{
	struct task_struct *t = (struct task_struct *)bpf_get_current_task();
	return BPF_CORE_READ(t, cred, euid.val);
}

static __always_inline void fill(struct hdr *h, __u32 type)
{
	struct task_struct *t = (struct task_struct *)bpf_get_current_task();
	__u64 id = bpf_get_current_pid_tgid();

	h->ts = bpf_ktime_get_ns();
	h->cgroup = bpf_get_current_cgroup_id();
	h->type = type;
	h->pid = id >> 32;
	h->tid = (__u32)id;
	h->ppid = BPF_CORE_READ(t, real_parent, tgid);
	h->uid = (__u32)bpf_get_current_uid_gid();
	h->euid = BPF_CORE_READ(t, cred, euid.val);
	bpf_get_current_comm(h->comm, sizeof(h->comm));
}

// A program was executed. Runs after the new image is in place, so comm,
// the file name and the argument block describe the new program.
SEC("tp_btf/sched_process_exec")
int BPF_PROG(on_exec, struct task_struct *p, int old_pid, struct linux_binprm *bprm)
{
	struct exec_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e) {
		__sync_fetch_and_add(&lost, 1);
		return 0;
	}
	fill(&e->h, EV_EXEC);
	e->filename[0] = 0;
	bpf_probe_read_kernel_str(e->filename, sizeof(e->filename), BPF_CORE_READ(bprm, filename));

	// argv as the kernel laid it out: strings separated by NUL.
	struct task_struct *t = (struct task_struct *)bpf_get_current_task();
	unsigned long start = BPF_CORE_READ(t, mm, arg_start);
	unsigned long end = BPF_CORE_READ(t, mm, arg_end);
	__u64 len = end - start;
	if (len > ARGS_LEN - 1)
		len = ARGS_LEN - 1;
	len &= ARGS_LEN - 1;
	e->args_len = 0;
	if (bpf_probe_read_user(e->args, len, (void *)start) == 0)
		e->args_len = len;
	bpf_ringbuf_submit(e, 0);
	return 0;
}

// A process ended (reported once, for its main thread).
SEC("tp_btf/sched_process_exit")
int BPF_PROG(on_exit, struct task_struct *p)
{
	__u64 id = bpf_get_current_pid_tgid();
	if ((id >> 32) != (__u32)id)
		return 0;
	struct exit_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e) {
		__sync_fetch_and_add(&lost, 1);
		return 0;
	}
	fill(&e->h, EV_EXIT);
	e->code = BPF_CORE_READ(p, exit_code);
	bpf_ringbuf_submit(e, 0);
	return 0;
}

// TCP: an outbound connection is attempted, or a socket starts listening.
// Both transitions happen inside the calling process's system call.
SEC("tp_btf/inet_sock_set_state")
int BPF_PROG(on_sock_state, struct sock *sk, int oldstate, int newstate)
{
	__u32 type;
	if (oldstate == TCP_CLOSE && newstate == TCP_SYN_SENT)
		type = EV_CONNECT;
	else if (newstate == TCP_LISTEN)
		type = EV_LISTEN;
	else
		return 0;

	__u16 family = BPF_CORE_READ(sk, __sk_common.skc_family);
	if (family != AF_INET && family != AF_INET6)
		return 0;
	struct net_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e) {
		__sync_fetch_and_add(&lost, 1);
		return 0;
	}
	fill(&e->h, type);
	e->family = family;
	e->sport = BPF_CORE_READ(sk, __sk_common.skc_num);
	e->dport = bpf_ntohs(BPF_CORE_READ(sk, __sk_common.skc_dport));
	e->pad = 0;
	__builtin_memset(e->saddr, 0, sizeof(e->saddr));
	__builtin_memset(e->daddr, 0, sizeof(e->daddr));
	if (family == AF_INET) {
		__be32 s = BPF_CORE_READ(sk, __sk_common.skc_rcv_saddr);
		__be32 d = BPF_CORE_READ(sk, __sk_common.skc_daddr);
		__builtin_memcpy(e->saddr, &s, 4);
		__builtin_memcpy(e->daddr, &d, 4);
	} else {
		BPF_CORE_READ_INTO(&e->saddr, sk, __sk_common.skc_v6_rcv_saddr.in6_u.u6_addr8);
		BPF_CORE_READ_INTO(&e->daddr, sk, __sk_common.skc_v6_daddr.in6_u.u6_addr8);
	}
	bpf_ringbuf_submit(e, 0);
	return 0;
}

static __always_inline bool watched(const char *path)
{
	for (__u32 i = 0; i < MAX_PREFIXES; i++) {
		__u32 key = i;
		struct prefix *p = bpf_map_lookup_elem(&watch, &key);
		if (!p || p->len == 0)
			return false;
		__u32 len = p->len;
		bool same = true;
		for (__u32 j = 0; j < PREFIX_LEN; j++) {
			if (j >= len)
				break;
			if (path[j] != p->s[j]) {
				same = false;
				break;
			}
		}
		if (same)
			return true;
	}
	return false;
}

// A file under a watched path is opened. Only absolute paths are matched:
// an open relative to a directory is not seen here.
static __always_inline int handle_open(const char *filename, int flags)
{
	__u32 zero = 0;
	struct open_event *e = bpf_map_lookup_elem(&scratch, &zero);
	if (!e)
		return 0;
	long n = bpf_probe_read_user_str(e->path, sizeof(e->path), filename);
	if (n <= 1 || e->path[0] != '/' || !watched(e->path))
		return 0;
	fill(&e->h, EV_OPEN);
	e->flags = flags;
	e->pad = 0;
	if (bpf_ringbuf_output(&events, e, sizeof(*e), 0))
		__sync_fetch_and_add(&lost, 1);
	return 0;
}

// Every system call passes through here, so everything that is not an open
// or a set*uid leaves on the first comparison. Raw tracepoints are used
// rather than the per-call syscalls:* ones: they need no tracefs, and they
// are called for every task.
SEC("tp_btf/sys_enter")
int BPF_PROG(on_sys_enter, struct pt_regs *regs, long id)
{
	switch (id) {
	case NR_openat:
		return handle_open((const char *)PT_REGS_PARM2_CORE_SYSCALL(regs), (int)PT_REGS_PARM3_CORE_SYSCALL(regs));
#ifdef NR_open
	case NR_open:
		return handle_open((const char *)PT_REGS_PARM1_CORE_SYSCALL(regs), (int)PT_REGS_PARM2_CORE_SYSCALL(regs));
#endif
	case NR_openat2: {
		// openat2 passes its flags in a struct; the first field holds them.
		__u64 flags = 0;
		bpf_probe_read_user(&flags, sizeof(flags), (void *)PT_REGS_PARM3_CORE_SYSCALL(regs));
		return handle_open((const char *)PT_REGS_PARM2_CORE_SYSCALL(regs), (int)flags);
	}
	case NR_setuid:
	case NR_setreuid:
	case NR_setresuid: {
		// Remember the effective uid going in; the exit probe compares.
		__u64 tid = bpf_get_current_pid_tgid();
		__u32 euid = current_euid();
		if (bpf_map_update_elem(&cred_before, &tid, &euid, BPF_ANY) == 0)
			__sync_fetch_and_add(&in_setuid, 1);
		return 0;
	}
	}
	return 0;
}

SEC("tp_btf/sys_exit")
int BPF_PROG(on_sys_exit, struct pt_regs *regs, long ret)
{
	if (in_setuid == 0)
		return 0;
	__u64 tid = bpf_get_current_pid_tgid();
	__u32 *before = bpf_map_lookup_elem(&cred_before, &tid);
	if (!before)
		return 0;
	__u32 old = *before;
	bpf_map_delete_elem(&cred_before, &tid);
	__sync_fetch_and_add(&in_setuid, -1);
	__u32 now = current_euid();
	if (ret != 0 || now == old)
		return 0;
	struct cred_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e) {
		__sync_fetch_and_add(&lost, 1);
		return 0;
	}
	fill(&e->h, EV_SETUID);
	e->old_euid = old;
	e->new_euid = now;
	bpf_ringbuf_submit(e, 0);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
