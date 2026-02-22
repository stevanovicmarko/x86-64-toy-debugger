# reg_read.s — Assembly test target for register reads.
#
# This program stores known values into registers and then executes
# int3 (SIGTRAP) so the debugger/test harness can read them back.
# No libc — uses raw syscalls and _start entry point.
#
# Build: gcc -o reg_read reg_read.s -nostdlib -no-pie

    .global _start
    .text

_start:
    # ── Trap 1: GPR read (r13 = 0xcafecafe) ──────────────────────
    movq    $0xcafecafe, %r13
    int3

    # ── Trap 2: Sub-register read (r13b = 42) ────────────────────
    # Clear r13 first, then set just the low byte.
    xorq    %r13, %r13
    movb    $42, %r13b
    int3

    # ── Trap 3: MM register read (mm0 = 0xba5eba11) ──────────────
    # Load a known 64-bit value into mm0 via memory.
    movq    $0xba5eba11, %r13
    movq    %r13, -8(%rsp)
    movq    -8(%rsp), %mm0
    int3

    # ── Trap 4: XMM register read (xmm0 = double 64.125) ─────────
    # IEEE 754 double for 64.125 = 0x4050080000000000
    movq    $0x4050080000000000, %r13
    movq    %r13, -8(%rsp)
    movsd   -8(%rsp), %xmm0
    int3

    # ── Trap 5: ST register read (st0 = double 64.125) ────────────
    emms
    fldl    -8(%rsp)
    int3

    # ── Exit via _exit(0) ─────────────────────────────────────────
    movq    $60, %rax
    xorq    %rdi, %rdi
    syscall
