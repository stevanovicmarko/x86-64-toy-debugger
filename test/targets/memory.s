# memory.s — Assembly test target for memory read/write.
#
# This program:
#   1. Stores 0xcafecafe in a local variable, writes its address
#      (8 bytes, little-endian) to stdout, then traps.
#   2. Zeroes a 32-byte buffer, writes its address to stdout, then traps.
#   3. After trap 2, the debugger has written into the buffer. The
#      program writes the buffer contents to stdout, then exits.
#
# No libc — uses raw syscalls and _start entry point.
#
# Build: gcc -o memory memory.s -nostdlib -no-pie

    .global _start
    .text

_start:
    # Allocate stack space:
    #   rsp-8   = known value (uint32 0xcafecafe, stored as 8 bytes)
    #   rsp-16  = address scratch for write(2) iov
    #   rsp-48  = 32-byte buffer for write test
    subq    $64, %rsp

    # ── Phase 1: memory read test ─────────────────────────────────
    # Store known value at rsp+48 (i.e., original_rsp - 8, but we
    # already decremented by 64, so it's rsp+56).
    movq    $0xcafecafe, %rax
    movq    %rax, 56(%rsp)

    # Write address of known value to stdout.
    # The address is rsp+56.
    leaq    56(%rsp), %rax
    movq    %rax, 48(%rsp)         # store address at rsp+48

    movq    $1, %rax               # sys_write
    movq    $1, %rdi               # fd = stdout
    leaq    48(%rsp), %rsi         # buf = &address
    movq    $8, %rdx               # count = 8 bytes
    syscall

    int3                            # Trap 1: debugger reads 0xcafecafe

    # ── Phase 2: memory write test ────────────────────────────────
    # Zero the 32-byte buffer at rsp+0..rsp+31.
    xorq    %rax, %rax
    movq    %rax, 0(%rsp)
    movq    %rax, 8(%rsp)
    movq    %rax, 16(%rsp)
    movq    %rax, 24(%rsp)

    # Write address of buffer to stdout.
    leaq    0(%rsp), %rax
    movq    %rax, 48(%rsp)         # store address at rsp+48

    movq    $1, %rax               # sys_write
    movq    $1, %rdi               # fd = stdout
    leaq    48(%rsp), %rsi         # buf = &address
    movq    $8, %rdx               # count = 8 bytes
    syscall

    int3                            # Trap 2: debugger writes to buffer

    # ── Phase 3: print buffer contents ────────────────────────────
    movq    $1, %rax               # sys_write
    movq    $1, %rdi               # fd = stdout
    leaq    0(%rsp), %rsi          # buf = buffer start
    movq    $32, %rdx              # count = 32 bytes
    syscall

    # ── Exit via _exit(0) ─────────────────────────────────────────
    movq    $60, %rax
    xorq    %rdi, %rdi
    syscall
