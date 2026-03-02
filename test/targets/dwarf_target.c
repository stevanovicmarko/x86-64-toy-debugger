/*
 * dwarf_target.c — Test target for DWARF debug information.
 *
 * This program provides multiple named functions that the debugger's
 * DWARF support can resolve by name and address. It follows the same
 * address-output + int3-stop protocol as anti_debugger.c.
 *
 * Protocol (communicates via stdout pipe):
 *   1. Writes the address of add_numbers (8 bytes LE) to stdout.
 *   2. Writes the address of multiply_numbers (8 bytes LE) to stdout.
 *   3. Raises SIGTRAP (int3) — debugger can inspect DWARF info.
 *   4. Calls add_numbers and multiply_numbers.
 *   5. Raises SIGTRAP again.
 *   6. Exits.
 *
 * Build: gcc -g -no-pie -o dwarf_target dwarf_target.c
 */

#include <stdint.h>
#include <unistd.h>

int add_numbers(int a, int b) {
    return a + b;
}

int multiply_numbers(int a, int b) {
    return a * b;
}

int main(void) {
    /* Write function addresses so the test can verify DWARF resolution. */
    uintptr_t addr;

    addr = (uintptr_t)add_numbers;
    write(STDOUT_FILENO, &addr, sizeof(addr));

    addr = (uintptr_t)multiply_numbers;
    write(STDOUT_FILENO, &addr, sizeof(addr));

    /* Trap 1: let the debugger inspect DWARF info. */
    __asm__ volatile("int3");

    /* Call the functions so they aren't optimized away. */
    volatile int result = 0;
    result += add_numbers(3, 4);
    result += multiply_numbers(5, 6);

    /* Trap 2: debugger can check state after calls. */
    __asm__ volatile("int3");

    return 0;
}
