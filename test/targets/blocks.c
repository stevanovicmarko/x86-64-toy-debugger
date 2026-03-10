/*
 * blocks.c — Test target for local variable scoping (lexical blocks).
 *
 * Three nested scopes redefine 'i' with different values. The debugger
 * should resolve 'i' to the correct innermost scope at each point.
 *
 * Build: gcc -g -no-pie -O0 -o blocks blocks.c
 */

#include <stdio.h>

int main(int argc, const char** argv) {
    int i = 1;
    fprintf(stderr, "%d", i);
    {
        int i = 2;
        fprintf(stderr, "%d", i);
        {
            int i = 3;
            fprintf(stderr, "%d", i);
        }
    }
    return 0;
}
