// run_endlessly loops forever without producing output.
// It's the Go equivalent of the C++ test target from the book:
//   int main() { volatile int i; while (true) i = 42; }
// We use a global volatile-style variable to prevent the compiler
// from optimizing away the loop.
package main

import "runtime"

var sink int

func main() {
	for {
		sink = 42
		runtime.Gosched()
	}
}
