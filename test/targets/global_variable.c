/*
 * global_variable.c — Test target for DWARF expression variable reading.
 *
 * A single global variable that is assigned three different values,
 * allowing the test to verify that the debugger can read it at each
 * point.
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
