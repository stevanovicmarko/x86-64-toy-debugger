package debugger

// StoppointMode describes the access type that triggers a hardware
// stoppoint (breakpoint or watchpoint). These map directly to the
// condition bits in the x86-64 DR7 control register.
type StoppointMode int

const (
	// StoppointModeExecute triggers on instruction execution (hardware breakpoint).
	StoppointModeExecute StoppointMode = iota

	// StoppointModeWrite triggers on data writes only (write watchpoint).
	StoppointModeWrite

	// StoppointModeReadWrite triggers on both data reads and writes (access watchpoint).
	StoppointModeReadWrite
)
