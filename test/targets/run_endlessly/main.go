// run_endlessly loops forever without producing output.
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
