// Kernel side of vigil's runtime events. Each probe writes one fixed-layout
// record to a ring buffer; internal/events/decode.go reads them.
//
// The file is self-contained on purpose: the few kernel structs it touches
// are declared here with only the fields it reads, and CO-RE relocates them
// against the running kernel's BTF. No vmlinux.h, no kernel headers.
//
// Needs: BTF (CONFIG_DEBUG_INFO_BTF) and the BPF ring buffer (Linux 5.8).
//
// Opens and privilege changes come in two versions. The fentry ones
// (on_file_open, on_open_refused, on_commit_creds) see what the kernel
// resolved and decided: the real path of the file opened, also for a
// relative name, "..", a symlink or a name changed in memory after the call
// began; opens the kernel refused, marked as such; and every change of the
// effective uid, also by running a setuid program or from inside the
// kernel. They need BPF trampolines (x86-64 from Linux 5.5, arm64 from
// 6.0). Where those cannot load, the loader falls back to the system-call
// probes (on_sys_enter, on_sys_exit), which read the name the program
// passed, see only absolute ones, and report every attempt without knowing
// whether it was refused. One of the two sets is loaded, never both.
// The escape probes (on_switch_ns, on_mount, on_ptrace, on_module_load,
// on_bpf_load, on_unix_connect) watch what a process in a container needs
// to reach its host. They are loaded one by one, after the rest, and a
// kernel that cannot take one (a function inlined or renamed) goes without
// that one alone.
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
#define BPF_MAP_TYPE_LRU_HASH 9
#define BPF_MAP_TYPE_RINGBUF 27
#define BPF_ANY 0
#define BPF_NOEXIST 1

// Tells libbpf's headers that kernel-style register names (di, si, ...) are
// in use, as they would be with a generated vmlinux.h.
#define __VMLINUX_H__

#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>
#include <bpf/bpf_endian.h>

#pragma clang attribute push(__attribute__((preserve_access_index)), apply_to = record)
typedef struct { __u32 val; } kuid_t;
typedef struct { int counter; } atomic_t;
struct cred { kuid_t uid; kuid_t euid; };
struct mm_struct { unsigned long arg_start; unsigned long arg_end; };
struct signal_struct { atomic_t live; };
struct ns_common { unsigned int inum; };
struct mnt_namespace { struct ns_common ns; };
struct uts_namespace { struct ns_common ns; };
struct ipc_namespace { struct ns_common ns; };
struct pid_namespace { struct ns_common ns; };
struct net { struct ns_common ns; };
struct cgroup_namespace { struct ns_common ns; };
struct nsproxy {
	struct uts_namespace *uts_ns;
	struct ipc_namespace *ipc_ns;
	struct mnt_namespace *mnt_ns;
	struct pid_namespace *pid_ns_for_children;
	struct net *net_ns;
	struct cgroup_namespace *cgroup_ns;
};
struct task_struct {
	int pid;
	int tgid;
	char comm[16];
	struct nsproxy *nsproxy;
	struct task_struct *real_parent;
	struct task_struct *group_leader;
	__u64 start_time;
	struct mm_struct *mm;
	const struct cred *cred;
	struct signal_struct *signal;
	int exit_code;
};
struct qstr { const unsigned char *name; };
struct dentry { struct qstr d_name; struct dentry *d_parent; };
struct path { void *mnt; struct dentry *dentry; };
struct module { char name[56]; };
struct file { struct path f_path; unsigned int f_flags; unsigned int f_mode; };
struct filename { const char *name; };
struct open_flags { int open_flag; };
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
#define O_CREAT 0100
#define FMODE_CREATED 0x100000
#define EPERM 1
#define EACCES 13
#define TCP_SYN_SENT 2
#define TCP_CLOSE 7
#define TCP_LISTEN 10

enum { EV_EXEC = 1, EV_EXIT = 2, EV_CONNECT = 3, EV_LISTEN = 4, EV_OPEN = 5, EV_SETUID = 6,
	EV_NAMESPACE = 7, EV_MOUNT = 8, EV_PTRACE = 9, EV_MODULE = 10, EV_UNIX = 11 };

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
// err is 0 for an open that happened, else the error it was refused with.
struct open_event { struct hdr h; __s32 flags; __s32 err; char path[PATH_LEN]; };
struct cred_event { struct hdr h; __u32 old_euid; __u32 new_euid; };
// changed and host are bit sets of NS_*: the namespaces that changed, and
// those of them that are now the host's.
struct ns_event { struct hdr h; __u32 changed; __u32 host; };
struct mount_event { struct hdr h; __u64 flags; char source[128]; char fstype[32]; char parent[64]; char name[64]; };
struct ptrace_event { struct hdr h; __u32 target_pid; __u32 pad; char target_comm[COMM_LEN]; };
// kind 1: a kernel module (name); kind 2: a BPF program (prog_type, name).
struct module_event { struct hdr h; __u32 kind; __u32 prog_type; char name[64]; };
struct unix_event { struct hdr h; char path[128]; };

enum { NS_MNT = 0, NS_UTS = 1, NS_IPC = 2, NS_PID = 3, NS_NET = 4, NS_CGROUP = 5 };

struct { __uint(type, BPF_MAP_TYPE_RINGBUF); __uint(max_entries, 8 << 20); } events SEC(".maps");

// Paths whose opens are reported, packed from index 0; len 0 ends the list.
struct prefix { __u32 len; char s[PREFIX_LEN]; };
struct { __uint(type, BPF_MAP_TYPE_ARRAY); __uint(max_entries, MAX_PREFIXES); __type(key, __u32); __type(value, struct prefix); } watch SEC(".maps");

// An open record is built here first, so opens that match no prefix never
// touch the ring buffer.
struct { __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY); __uint(max_entries, 1); __type(key, __u32); __type(value, struct open_event); } scratch SEC(".maps");

// Effective uid of a thread on entry to a set*uid call.
struct { __uint(type, BPF_MAP_TYPE_HASH); __uint(max_entries, 4096); __type(key, __u64); __type(value, __u32); } cred_before SEC(".maps");

// Processes whose end was reported: two threads ending at the same moment
// can both see the process's last thread gone (see on_exit), and only the
// first to add the process here reports it. The start time keeps a reused
// pid apart; old entries simply age out.
struct exit_key { __u32 tgid; __u32 pad; __u64 start; };
struct { __uint(type, BPF_MAP_TYPE_LRU_HASH); __uint(max_entries, 4096); __type(key, struct exit_key); __type(value, __u8); } exited SEC(".maps");

// The host's namespaces by NS_*, as inode numbers: the agent reads them
// from /proc/1/ns before attaching on_switch_ns.
struct { __uint(type, BPF_MAP_TYPE_ARRAY); __uint(max_entries, 8); __type(key, __u32); __type(value, __u32); } host_ns SEC(".maps");

// A unix socket address is built here before it is matched.
struct { __uint(type, BPF_MAP_TYPE_PERCPU_ARRAY); __uint(max_entries, 1); __type(key, __u32); __type(value, struct unix_event); } unix_scratch SEC(".maps");

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

// A process ended: reported once, when its last thread exits. That is not
// always the main thread, which can end first while the others run on.
// do_exit counts the thread off signal->live before this tracepoint, so
// live reads 0 for the last one, but also for a thread that counted itself
// off just before the last one did and reaches the tracepoint after it.
SEC("tp_btf/sched_process_exit")
int BPF_PROG(on_exit, struct task_struct *p)
{
	if (BPF_CORE_READ(p, signal, live.counter) != 0)
		return 0;
	struct exit_key k = {.tgid = BPF_CORE_READ(p, tgid), .start = BPF_CORE_READ(p, group_leader, start_time)};
	__u8 one = 1;
	if (bpf_map_update_elem(&exited, &k, &one, BPF_NOEXIST) != 0)
		return 0; // already reported
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

// A file under a watched path is opened, as the kernel resolved it: the
// path is built from the opened file itself (bpf_d_path), relative to the
// process's root, so a container sees its own paths. Called for every open
// on the host, so everything else leaves after the prefix comparison.
// It runs on the way out, so an open a security module (AppArmor, SELinux)
// refuses here is not reported as opened; on_open_refused reports it.
SEC("fexit/security_file_open")
int BPF_PROG(on_file_open, struct file *file, int ret)
{
	if (ret != 0)
		return 0;
	__u32 zero = 0;
	struct open_event *e = bpf_map_lookup_elem(&scratch, &zero);
	if (!e)
		return 0;
	long n = bpf_d_path(&file->f_path, e->path, sizeof(e->path));
	if (n <= 1 || e->path[0] != '/' || !watched(e->path))
		return 0; // too long, not a path (pipe:, anon_inode:), or not watched
	fill(&e->h, EV_OPEN);
	// The kernel clears O_CREAT from f_flags once the file exists; f_mode
	// says it was made by this open.
	int flags = file->f_flags;
	if (file->f_mode & FMODE_CREATED)
		flags |= O_CREAT;
	e->flags = flags;
	e->err = 0;
	if (bpf_ringbuf_output(&events, e, sizeof(*e), 0))
		__sync_fetch_and_add(&lost, 1);
	return 0;
}

// An open the kernel refused for lack of permission: a user without the
// rights trying to read /etc/shadow never gets as far as security_file_open,
// because the permission check comes first. do_filp_open is where every
// open by name ends (open, openat, openat2, io_uring), and it returns the
// error. There is no opened file to resolve, so the name is the one the
// program gave, copied into the kernel when the call began; a relative name
// is not matched. Other errors (no such file, ...) are not reported.
SEC("fexit/do_filp_open")
int BPF_PROG(on_open_refused, int dfd, struct filename *pathname, const struct open_flags *op, struct file *ret)
{
	long err = (long)ret;
	if (err != -EACCES && err != -EPERM)
		return 0; // opened (a pointer), or another error
	__u32 zero = 0;
	struct open_event *e = bpf_map_lookup_elem(&scratch, &zero);
	if (!e)
		return 0;
	long n = bpf_probe_read_kernel_str(e->path, sizeof(e->path), BPF_CORE_READ(pathname, name));
	if (n <= 1 || e->path[0] != '/' || !watched(e->path))
		return 0;
	fill(&e->h, EV_OPEN);
	e->flags = BPF_CORE_READ(op, open_flag);
	e->err = -err;
	if (bpf_ringbuf_output(&events, e, sizeof(*e), 0))
		__sync_fetch_and_add(&lost, 1);
	return 0;
}

// The effective uid of a task changes: every set*uid call, running a setuid
// program, and a kernel exploit that hands itself root all go through
// commit_creds. Only a change of the effective uid is reported. The header
// still carries the old credentials; for a setuid program its name is still
// the old program's, the exec event follows.
SEC("fentry/commit_creds")
int BPF_PROG(on_commit_creds, struct cred *new)
{
	__u32 old = current_euid();
	__u32 now = BPF_CORE_READ(new, euid.val);
	if (now == old)
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

// The fallback for opens: the name the program passed. Only absolute paths
// are matched, so an open relative to a directory is not seen here.
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
	e->err = 0;
	if (bpf_ringbuf_output(&events, e, sizeof(*e), 0))
		__sync_fetch_and_add(&lost, 1);
	return 0;
}

// The fallback probes, loaded only where the fentry ones cannot be.
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

// --- Escape probes ---

#define NS_CHECK(bit, field)                                                         \
	{                                                                                \
		__u32 a = BPF_CORE_READ(old, field, ns.inum);                                \
		__u32 b = BPF_CORE_READ(new, field, ns.inum);                                \
		if (a != b) {                                                                \
			changed |= 1 << bit;                                                     \
			__u32 k = bit;                                                           \
			__u32 *h = bpf_map_lookup_elem(&host_ns, &k);                            \
			if (h && *h != 0 && *h == b)                                             \
				host |= 1 << bit;                                                    \
		}                                                                            \
	}

// A process joins other namespaces (setns, nsenter) or makes new ones
// (unshare). Both end here; so does every exit, with new == NULL, which
// leaves at once. A process in a container whose new namespaces are the
// host's has left its container.
SEC("fentry/switch_task_namespaces")
int BPF_PROG(on_switch_ns, struct task_struct *p, struct nsproxy *new)
{
	if (!new)
		return 0;
	struct nsproxy *old = BPF_CORE_READ(p, nsproxy);
	if (!old)
		return 0;
	__u32 changed = 0, host = 0;
	NS_CHECK(NS_MNT, mnt_ns)
	NS_CHECK(NS_UTS, uts_ns)
	NS_CHECK(NS_IPC, ipc_ns)
	NS_CHECK(NS_PID, pid_ns_for_children)
	NS_CHECK(NS_NET, net_ns)
	NS_CHECK(NS_CGROUP, cgroup_ns)
	if (!changed)
		return 0;
	struct ns_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e) {
		__sync_fetch_and_add(&lost, 1);
		return 0;
	}
	fill(&e->h, EV_NAMESPACE);
	e->changed = changed;
	e->host = host;
	bpf_ringbuf_submit(e, 0);
	return 0;
}

// A mount by the mount(2) system call: every argument is a kernel copy by
// now. The mount point is given as its last two path components; the full
// path would need bpf_d_path, which the kernel allows only in a few places.
// The newer fsopen/fsmount calls do not pass here.
SEC("fentry/path_mount")
int BPF_PROG(on_mount, const char *dev_name, struct path *path, const char *type_page, unsigned long flags)
{
	struct mount_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e) {
		__sync_fetch_and_add(&lost, 1);
		return 0;
	}
	fill(&e->h, EV_MOUNT);
	e->flags = flags;
	e->source[0] = 0;
	e->fstype[0] = 0;
	e->parent[0] = 0;
	e->name[0] = 0;
	if (dev_name)
		bpf_probe_read_kernel_str(e->source, sizeof(e->source), dev_name);
	if (type_page)
		bpf_probe_read_kernel_str(e->fstype, sizeof(e->fstype), type_page);
	struct dentry *d = BPF_CORE_READ(path, dentry);
	bpf_probe_read_kernel_str(e->name, sizeof(e->name), BPF_CORE_READ(d, d_name.name));
	struct dentry *up = BPF_CORE_READ(d, d_parent);
	if (up && up != d)
		bpf_probe_read_kernel_str(e->parent, sizeof(e->parent), BPF_CORE_READ(up, d_name.name));
	bpf_ringbuf_submit(e, 0);
	return 0;
}

// A process attached to another with ptrace (PTRACE_ATTACH or SEIZE): it
// can now read and change the other's memory. Reported once it succeeded.
SEC("fexit/ptrace_attach")
int BPF_PROG(on_ptrace, struct task_struct *task, long request, unsigned long addr, unsigned long flags, int ret)
{
	if (ret != 0)
		return 0;
	struct ptrace_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e) {
		__sync_fetch_and_add(&lost, 1);
		return 0;
	}
	fill(&e->h, EV_PTRACE);
	e->target_pid = BPF_CORE_READ(task, tgid);
	e->pad = 0;
	BPF_CORE_READ_STR_INTO(&e->target_comm, task, comm);
	bpf_ringbuf_submit(e, 0);
	return 0;
}

// A kernel module was loaded: code running in the kernel itself.
SEC("tp_btf/module_load")
int BPF_PROG(on_module_load, struct module *mod)
{
	struct module_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e) {
		__sync_fetch_and_add(&lost, 1);
		return 0;
	}
	fill(&e->h, EV_MODULE);
	e->kind = 1;
	e->prog_type = 0;
	e->name[0] = 0;
	BPF_CORE_READ_STR_INTO(&e->name, mod, name);
	bpf_ringbuf_submit(e, 0);
	return 0;
}

// A BPF program is being loaded. The attributes are the system call's
// (union bpf_attr, kernel copy), laid out by the stable user-space API:
// prog_type first, prog_name at byte 48.
SEC("fentry/bpf_prog_load")
int BPF_PROG(on_bpf_load, void *attr)
{
	struct module_event *e = bpf_ringbuf_reserve(&events, sizeof(*e), 0);
	if (!e) {
		__sync_fetch_and_add(&lost, 1);
		return 0;
	}
	fill(&e->h, EV_MODULE);
	e->kind = 2;
	e->prog_type = 0;
	__builtin_memset(e->name, 0, sizeof(e->name));
	bpf_probe_read_kernel(&e->prog_type, sizeof(e->prog_type), attr);
	bpf_probe_read_kernel(e->name, 16, (char *)attr + 48);
	e->name[15] = 0;
	bpf_ringbuf_submit(e, 0);
	return 0;
}

static __always_inline bool ends_with(const char *p, __u32 n, const char *s, __u32 k)
{
	if (n < k)
		return false;
	for (__u32 i = 0; i < k; i++) {
		if (p[(n - k + i) & 127] != s[i])
			return false;
	}
	return true;
}

// A connection to a unix socket named like a container runtime's API
// (docker.sock, containerd.sock, ...): whoever talks to it can start a
// privileged container. Every other unix connection leaves after the name
// comparison; abstract sockets (no file) at once.
SEC("fentry/unix_stream_connect")
int BPF_PROG(on_unix_connect, void *sock, void *uaddr, int addr_len)
{
	__u32 zero = 0;
	struct unix_event *e = bpf_map_lookup_elem(&unix_scratch, &zero);
	if (!e)
		return 0;
	__builtin_memset(e->path, 0, sizeof(e->path));
	// struct sockaddr_un: a 2-byte family, then up to 108 bytes of path.
	if (bpf_probe_read_kernel(e->path, 108, (char *)uaddr + 2) != 0 || e->path[0] == 0)
		return 0;
	__u32 n = 0;
	for (__u32 i = 0; i < 108; i++) {
		if (e->path[i] == 0)
			break;
		n++;
	}
	if (!(ends_with(e->path, n, "docker.sock", 11) || ends_with(e->path, n, "containerd.sock", 15) ||
	      ends_with(e->path, n, "crio.sock", 9) || ends_with(e->path, n, "podman.sock", 11)))
		return 0;
	fill(&e->h, EV_UNIX);
	if (bpf_ringbuf_output(&events, e, sizeof(*e), 0))
		__sync_fetch_and_add(&lost, 1);
	return 0;
}

char LICENSE[] SEC("license") = "Dual BSD/GPL";
