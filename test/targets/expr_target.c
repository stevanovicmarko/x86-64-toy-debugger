/*
 * expr_target.c — Test target for expression evaluation.
 *
 * Provides various functions with different parameter and return types
 * to test the debugger's expression evaluator and inferior function calls.
 * Uses debugger-themed names throughout.
 *
 * Build: gcc -g -no-pie -o expr_target expr_target.c
 */

#include <stdio.h>
#include <string.h>
#include <stdint.h>

/* --- Simple functions with different argument types --- */

int decode_signal(int signal_id) {
    return signal_id * 10 + 1;
}

double compute_offset(double base) {
    return base * 2.5;
}

const char* identify_register(const char* name) {
    if (strcmp(name, "rax") == 0) return "accumulator";
    if (strcmp(name, "rsp") == 0) return "stack pointer";
    if (strcmp(name, "rip") == 0) return "instruction pointer";
    return "unknown";
}

char encode_flag(char flag) {
    return flag + 1;
}

/* --- Struct types and functions returning structs --- */

struct breakpoint_info {
    int id;
    uint64_t address;
};

struct breakpoint_info create_breakpoint(int id, uint64_t address) {
    struct breakpoint_info bp;
    bp.id = id;
    bp.address = address;
    return bp;
}

/* --- Large struct (passed on stack, returned via memory) --- */

struct register_set {
    uint64_t rax;
    uint64_t rbx;
    uint64_t rcx;
};

struct register_set snapshot_registers(uint64_t a, uint64_t b, uint64_t c) {
    struct register_set regs;
    regs.rax = a;
    regs.rbx = b;
    regs.rcx = c;
    return regs;
}

/* --- Void function (no return value) --- */

static int last_signal = 0;

void dispatch_signal(int sig) {
    last_signal = sig;
}

int get_last_signal(void) {
    return last_signal;
}

/* --- Function with multiple integer args (tests rdi, rsi, rdx, rcx, r8, r9) --- */

int sum_registers(int a, int b, int c, int d, int e, int f) {
    return a + b + c + d + e + f;
}

/* --- Global variables for passing as arguments --- */

int g_breakpoint_count = 5;
const char* g_target_name = "test_inferior";

int main(void) {
    /* Set a marker for the debugger to break on. */
    volatile int ready = 1;
    (void)ready;
    return 0;
}
