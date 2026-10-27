// The few kernel UAPI definitions tracelens.bpf.c needs, so it builds without kernel
// headers or vmlinux.h (macOS has neither). Values match include/uapi/linux/bpf.h.
#pragma once

typedef unsigned char __u8;
typedef short __s16;
typedef unsigned short __u16;
typedef int __s32;
typedef unsigned int __u32;
typedef long long __s64;
typedef unsigned long long __u64;
typedef _Bool bool;
// bpf_helper_defs.h refers to these in helper prototypes we don't call.
typedef __u16 __be16;
typedef __u32 __be32;
typedef __u32 __wsum;

enum bpf_map_type {
	BPF_MAP_TYPE_ARRAY = 2,
	BPF_MAP_TYPE_PERCPU_HASH = 5,
	BPF_MAP_TYPE_PERCPU_ARRAY = 6,
	BPF_MAP_TYPE_LRU_HASH = 9,
	BPF_MAP_TYPE_RINGBUF = 27,
};

enum {
	BPF_ANY = 0,
	BPF_NOEXIST = 1,
};

#include "bpf_helpers.h"
