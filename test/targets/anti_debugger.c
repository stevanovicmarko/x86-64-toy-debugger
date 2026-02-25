/*
 * anti_debugger.c — Test target for hardware breakpoints and watchpoints.
 *
 * This program demonstrates why hardware breakpoints are needed:
 * it checksums its own function code, detecting software breakpoint
 * (0xCC) injection. Hardware breakpoints are invisible to this check.
 *
 * Protocol (communicates via stdout pipe):
 *   1. Writes the address of an_innocent_function (8 bytes LE) to stdout.
 *   2. Raises SIGTRAP (inline asm int3).
 *   3. Computes a checksum of an_innocent_function's bytes.
 *   4. If checksum matches: calls an_innocent_function → prints "pineapple".
 *      If checksum differs: prints "pepperoni" (detected tampering).
 *   5. Raises SIGTRAP again.
 *   6. Loops back to step 3.
 *
 * Build: gcc -o anti_debugger anti_debugger.c -no-pie
 */

#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <unistd.h>

/* Mark the start and end so the debugger (and the program itself)
   can compute the function's byte range. */
void an_innocent_function(void);
extern char an_innocent_function_end[];

void an_innocent_function(void) {
    const char msg[] = "pineapple\n";
    write(STDOUT_FILENO, msg, sizeof(msg) - 1);
}

/* We place a label right after the function body using inline asm.
   This lets us measure the exact byte size of an_innocent_function. */
__asm__(".globl an_innocent_function_end\n"
        "an_innocent_function_end:");

static uint32_t checksum_function(void) {
    const uint8_t *start = (const uint8_t *)an_innocent_function;
    const uint8_t *end   = (const uint8_t *)an_innocent_function_end;
    uint32_t sum = 0;
    for (const uint8_t *p = start; p < end; p++) {
        sum += *p;
    }
    return sum;
}

int main(void) {
    /* Disable stdout buffering so writes are immediately visible. */
    setvbuf(stdout, NULL, _IONBF, 0);

    /* Write the address of an_innocent_function to stdout (8 bytes LE). */
    uintptr_t addr = (uintptr_t)an_innocent_function;
    write(STDOUT_FILENO, &addr, sizeof(addr));

    /* Compute the "clean" checksum before any breakpoints. */
    uint32_t original_checksum = checksum_function();

    /* Trap 1: let the debugger set up breakpoints. */
    __asm__ volatile("int3");

    /* Loop: check for tampering, call or skip, trap again. */
    for (int i = 0; i < 2; i++) {
        uint32_t current = checksum_function();
        if (current == original_checksum) {
            an_innocent_function();
        } else {
            const char msg[] = "pepperoni\n";
            write(STDOUT_FILENO, msg, sizeof(msg) - 1);
        }

        /* Trap: let the debugger inspect / modify state. */
        __asm__ volatile("int3");
    }

    return 0;
}
