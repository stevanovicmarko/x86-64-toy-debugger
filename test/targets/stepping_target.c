/*
 * stepping_target.c — Test target for source-level stepping.
 *
 * Provides multiple small functions for testing StepIn, StepOver,
 * and StepOut. Follows the address-output + int3-stop protocol.
 *
 * Protocol (communicates via stdout pipe):
 *   1. Writes the address of add (8 bytes LE) to stdout.
 *   2. Raises SIGTRAP (int3) — debugger can set breakpoints/step.
 *   3. Calls add and mul in sequence.
 *   4. Raises SIGTRAP again.
 *   5. Exits.
 *
 * Build: gcc -g -no-pie -o stepping_target stepping_target.c
 */

#include <stdint.h>
#include <unistd.h>

int add(int a, int b) {
    return a + b;
}

int mul(int a, int b) {
    return a * b;
}

int main(void) {
    /* Write address of add so tests can verify DWARF resolution. */
    uintptr_t addr = (uintptr_t)add;
    write(STDOUT_FILENO, &addr, sizeof(addr));

    /* Trap 1: let the debugger set up before the calls. */
    __asm__ volatile("int3");

    int x = add(3, 4);         /* line 37 */
    int y = mul(5, 6);         /* line 38 */
    volatile int z = x + y;    /* line 39 */
    (void)z;

    /* Trap 2: debugger can check state after calls. */
    __asm__ volatile("int3");

    return 0;
}
