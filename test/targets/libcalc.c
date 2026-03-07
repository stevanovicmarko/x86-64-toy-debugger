// libcalc.c — A simple shared library for testing dynamic library support.
// It defines a function that references a variable from the main program.

extern int calc_client_value;

int calc_check_threshold(void) {
    return calc_client_value > 50;
}
