# reg_write.s — Assembly test target for register writes.
#
# This program traps at each stage so the debugger can write a register,
# then prints the register value so the test can verify it via stdout.
# Uses libc (printf, fflush) — linked with gcc.
#
# Build: gcc -o reg_write reg_write.s -no-pie

    .global main
    .text

main:
    pushq   %rbp
    movq    %rsp, %rbp

    # ── Trap 1: Debugger writes rsi, we print it ─────────────────
    int3
    # After this trap, the debugger has written a value into rsi.
    leaq    hex_fmt(%rip), %rdi
    # rsi already set by debugger
    xorq    %rax, %rax          # no vector args
    call    printf
    # Flush stdout so the pipe reader sees it immediately.
    movq    stdout(%rip), %rdi
    call    fflush

    # ── Trap 2: Debugger writes mm0, we print it ─────────────────
    int3
    # Move mm0 → rsi for printf.
    movq    %mm0, %rsi
    leaq    hex_fmt(%rip), %rdi
    xorq    %rax, %rax
    call    printf
    movq    stdout(%rip), %rdi
    call    fflush

    # ── Trap 3: Debugger writes xmm0, we print it as double ──────
    int3
    # xmm0 has been written by the debugger as a double.
    leaq    float_fmt(%rip), %rdi
    movq    $1, %rax            # one vector arg in xmm0
    call    printf
    movq    stdout(%rip), %rdi
    call    fflush

    # ── Trap 4: Final sync trap ───────────────────────────────────
    int3

    # Exit.
    xorq    %rax, %rax
    popq    %rbp
    ret

    .section .rodata
hex_fmt:
    .asciz "0x%lx"
float_fmt:
    .asciz "%g"
