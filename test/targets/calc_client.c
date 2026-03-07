// calc_client.c — Main program that links against libcalc.so.
// Used to test shared library tracing in the debugger.

#include <stdio.h>

int calc_client_value = 100;

int calc_check_threshold(void);

int main(void) {
    printf("Client value: %d\n", calc_client_value);
    int above = calc_check_threshold();
    printf("Above threshold: %s\n", above ? "true" : "false");
    return 0;
}
