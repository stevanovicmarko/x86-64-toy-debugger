/*
 * global_variable.c — Test target for typed variable reading.
 *
 * Tests: global integers, structs with bitfields, pointers, arrays,
 * and nested struct access via the debugger's variable commands.
 *
 * Build: gcc -g -no-pie -o global_variable global_variable.c
 */

#include <stdint.h>

uint64_t g_int = 0;

int main(void) {
    g_int = 1;
    g_int = 42;
    return 0;
}

/* Debugger-themed test types instead of the book's "cat" examples. */
struct register_entry {
    const char* name;
    int size : 5;
    int type_id : 3;
};

struct target_info {
    const char* name;
    int pid;
    struct register_entry* regs;
    int num_regs;
};

struct register_entry rax_entry = { "rax", 8, 1 };
struct register_entry rbx_entry = { "rbx", 8, 2 };
struct register_entry rcx_entry = { "rcx", 4, 3 };
struct register_entry reg_list[] = { { "rax", 8, 1 }, { "rbx", 8, 2 }, { "rcx", 4, 3 } };
struct target_info debuggee = { "test_program", 1234, reg_list, 3 };
