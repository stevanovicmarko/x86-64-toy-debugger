# hello_toydbg.s — Assembly test target for breakpoint tests.
#
# This program writes "Hello, toydbg!\n" to stdout and exits.
# No libc — uses raw syscalls and _start entry point.
# The non-PIE build ensures _start has a fixed virtual address,
# which lets tests set breakpoints at known addresses.
#
# Build: gcc -o hello_toydbg hello_toydbg.s -nostdlib -no-pie

    .global _start
    .text

_start:
    # ── write(1, msg, 15) ─────────────────────────────────────────
    movq    $1, %rax        # syscall number: write
    movq    $1, %rdi        # fd: stdout
    leaq    msg(%rip), %rsi # buf: address of message
    movq    $15, %rdx       # count: length of message
    syscall

    # ── exit(0) ───────────────────────────────────────────────────
    movq    $60, %rax       # syscall number: exit
    xorq    %rdi, %rdi      # status: 0
    syscall

    .section .rodata
msg:
    .ascii "Hello, toydbg!\n"
