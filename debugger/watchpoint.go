package debugger

import (
	"encoding/binary"
	"sync/atomic"
)

// watchpointIDCounter is the global counter for assigning unique watchpoint IDs.
var watchpointIDCounter int32

// nextWatchpointID returns the next unique watchpoint ID.
func nextWatchpointID() int32 {
	return atomic.AddInt32(&watchpointIDCounter, 1)
}

// Watchpoint represents a data watchpoint set via a hardware debug
// register. Unlike software breakpoints, watchpoints trigger on memory
// reads or writes rather than instruction execution.
type Watchpoint struct {
	id                    int32
	proc                  *Process
	address               uint64
	mode                  StoppointMode
	size                  int
	isEnabled             bool
	hardwareRegisterIndex int
	data                  uint64
	previousData          uint64
}

// ID returns the unique identifier for this watchpoint.
func (w *Watchpoint) ID() int32 {
	return w.id
}

// Address returns the watched memory address.
func (w *Watchpoint) Address() uint64 {
	return w.address
}

// Mode returns the access mode (Write or ReadWrite).
func (w *Watchpoint) Mode() StoppointMode {
	return w.mode
}

// Size returns the number of bytes being watched (1, 2, 4, or 8).
func (w *Watchpoint) Size() int {
	return w.size
}

// IsEnabled returns whether the watchpoint is currently active.
func (w *Watchpoint) IsEnabled() bool {
	return w.isEnabled
}

// Data returns the current value at the watched address (read at the
// most recent watchpoint hit or when the watchpoint was created).
func (w *Watchpoint) Data() uint64 {
	return w.data
}

// PreviousData returns the value at the watched address before the
// most recent update (i.e., the old value before the write that
// triggered the watchpoint).
func (w *Watchpoint) PreviousData() uint64 {
	return w.previousData
}

// UpdateData reads the current value at the watched address and
// shifts the old value into PreviousData. This should be called
// each time the watchpoint triggers to capture the new value.
func (w *Watchpoint) UpdateData() error {
	buf, err := w.proc.ReadMemory(w.address, w.size)
	if err != nil {
		return err
	}
	w.previousData = w.data
	// Read the value in little-endian, zero-extending to uint64.
	switch w.size {
	case 1:
		w.data = uint64(buf[0])
	case 2:
		w.data = uint64(binary.LittleEndian.Uint16(buf))
	case 4:
		w.data = uint64(binary.LittleEndian.Uint32(buf))
	case 8:
		w.data = binary.LittleEndian.Uint64(buf)
	}
	return nil
}

// Enable activates the watchpoint by programming the debug register.
func (w *Watchpoint) Enable() error {
	idx, err := w.proc.SetWatchpoint(w.address, w.mode, w.size)
	if err != nil {
		return err
	}
	w.hardwareRegisterIndex = idx
	w.isEnabled = true
	return nil
}

// Disable deactivates the watchpoint by clearing the debug register.
func (w *Watchpoint) Disable() error {
	if err := w.proc.ClearHardwareStoppoint(w.hardwareRegisterIndex); err != nil {
		return err
	}
	w.isEnabled = false
	return nil
}

// watchpointCollection is an ordered collection of watchpoints.
type watchpointCollection struct {
	points []*Watchpoint
}

func (c *watchpointCollection) push(wp *Watchpoint) {
	c.points = append(c.points, wp)
}

func (c *watchpointCollection) containsAddress(addr uint64) bool {
	for _, wp := range c.points {
		if wp.address == addr {
			return true
		}
	}
	return false
}

func (c *watchpointCollection) getByID(id int32) (*Watchpoint, bool) {
	for _, wp := range c.points {
		if wp.id == id {
			return wp, true
		}
	}
	return nil, false
}

func (c *watchpointCollection) removeByID(id int32) bool {
	for i, wp := range c.points {
		if wp.id == id {
			c.points = append(c.points[:i], c.points[i+1:]...)
			return true
		}
	}
	return false
}

func (c *watchpointCollection) size() int {
	return len(c.points)
}

func (c *watchpointCollection) forEach(fn func(*Watchpoint)) {
	for _, wp := range c.points {
		fn(wp)
	}
}
