package debugger

import "sync/atomic"

// breakpointIDCounter is the global counter for assigning unique IDs.
var breakpointIDCounter int32

// BreakpointSite represents a breakpoint at a specific address.
//
// Software breakpoints work by overwriting the first byte of an instruction
// with 0xCC (int3). The original byte is saved so it can be restored.
//
// Hardware breakpoints use x86-64 debug registers (DR0–DR3) and are
// invisible to the tracee's memory — making them undetectable by
// anti-debugging code that checksums its own instructions.
type BreakpointSite struct {
	id                    int32
	pid                   int
	proc                  *Process
	address               uint64
	isEnabled             bool
	savedData             byte
	isHardware            bool
	isInternal            bool
	hardwareRegisterIndex int
}

// ID returns the unique identifier for this breakpoint site.
func (b *BreakpointSite) ID() int32 {
	return b.id
}

// Address returns the address where this breakpoint is set.
func (b *BreakpointSite) Address() uint64 {
	return b.address
}

// IsEnabled returns whether the breakpoint is currently active.
func (b *BreakpointSite) IsEnabled() bool {
	return b.isEnabled
}

// IsHardware returns true if this breakpoint uses a debug register
// instead of int3 injection.
func (b *BreakpointSite) IsHardware() bool {
	return b.isHardware
}

// IsInternal returns true if this breakpoint was created internally
// by the debugger (e.g., for step-over logic) and should be hidden
// from the user.
func (b *BreakpointSite) IsInternal() bool {
	return b.isInternal
}

// AtAddress reports whether this breakpoint is at the given address.
func (b *BreakpointSite) AtAddress(addr uint64) bool {
	return b.address == addr
}

// InRange reports whether this breakpoint's address falls within
// [low, high).
func (b *BreakpointSite) InRange(low, high uint64) bool {
	return b.address >= low && b.address < high
}

// Enable activates the breakpoint. For software breakpoints, this
// overwrites the first byte at the address with 0xCC (int3). For
// hardware breakpoints, this programs a debug register via
// Process.SetHardwareBreakpoint.
func (b *BreakpointSite) Enable() error {
	if b.isHardware {
		idx, err := b.proc.SetHardwareBreakpoint(b.address)
		if err != nil {
			return err
		}
		b.hardwareRegisterIndex = idx
		b.isEnabled = true
		return nil
	}

	data, err := ptracePeekData(b.pid, b.address)
	if err != nil {
		return newErrorf("breakpoint enable: peek failed at 0x%x: %v", b.address, err)
	}
	b.savedData = byte(data & 0xFF)
	dataWithInt3 := (data & ^uint64(0xFF)) | 0xCC
	if err := ptracePokeData(b.pid, b.address, dataWithInt3); err != nil {
		return newErrorf("breakpoint enable: poke failed at 0x%x: %v", b.address, err)
	}
	b.isEnabled = true
	return nil
}

// Disable deactivates the breakpoint. For software breakpoints, this
// restores the original byte. For hardware breakpoints, this clears
// the debug register.
func (b *BreakpointSite) Disable() error {
	if b.isHardware {
		if err := b.proc.ClearHardwareStoppoint(b.hardwareRegisterIndex); err != nil {
			return err
		}
		b.isEnabled = false
		return nil
	}

	data, err := ptracePeekData(b.pid, b.address)
	if err != nil {
		return newErrorf("breakpoint disable: peek failed at 0x%x: %v", b.address, err)
	}
	restoredData := (data & ^uint64(0xFF)) | uint64(b.savedData)
	if err := ptracePokeData(b.pid, b.address, restoredData); err != nil {
		return newErrorf("breakpoint disable: poke failed at 0x%x: %v", b.address, err)
	}
	b.isEnabled = false
	return nil
}

// breakpointSiteCollection is an ordered collection of breakpoint sites.
type breakpointSiteCollection struct {
	sites []*BreakpointSite
}

func (c *breakpointSiteCollection) push(site *BreakpointSite) {
	c.sites = append(c.sites, site)
}

func (c *breakpointSiteCollection) containsID(id int32) bool {
	for _, s := range c.sites {
		if s.id == id {
			return true
		}
	}
	return false
}

func (c *breakpointSiteCollection) containsAddress(addr uint64) bool {
	for _, s := range c.sites {
		if s.address == addr {
			return true
		}
	}
	return false
}

func (c *breakpointSiteCollection) enabledAtAddress(addr uint64) *BreakpointSite {
	for _, s := range c.sites {
		if s.address == addr && s.isEnabled {
			return s
		}
	}
	return nil
}

// enabledSoftwareAtAddress returns the enabled *software* breakpoint at
// the given address, or nil. Hardware breakpoints are excluded because
// they don't inject 0xCC and require different PC adjustment logic.
func (c *breakpointSiteCollection) enabledSoftwareAtAddress(addr uint64) *BreakpointSite {
	for _, s := range c.sites {
		if s.address == addr && s.isEnabled && !s.isHardware {
			return s
		}
	}
	return nil
}

func (c *breakpointSiteCollection) getByID(id int32) (*BreakpointSite, bool) {
	for _, s := range c.sites {
		if s.id == id {
			return s, true
		}
	}
	return nil, false
}

func (c *breakpointSiteCollection) getByAddress(addr uint64) (*BreakpointSite, bool) {
	for _, s := range c.sites {
		if s.address == addr {
			return s, true
		}
	}
	return nil, false
}

func (c *breakpointSiteCollection) removeByID(id int32) bool {
	for i, s := range c.sites {
		if s.id == id {
			c.sites = append(c.sites[:i], c.sites[i+1:]...)
			return true
		}
	}
	return false
}

func (c *breakpointSiteCollection) removeByAddress(addr uint64) bool {
	for i, s := range c.sites {
		if s.address == addr {
			c.sites = append(c.sites[:i], c.sites[i+1:]...)
			return true
		}
	}
	return false
}

// getInRegion returns all enabled *software* breakpoint sites whose
// address falls within [low, high). Hardware breakpoints are excluded
// because they don't modify the tracee's memory.
func (c *breakpointSiteCollection) getInRegion(low, high uint64) []*BreakpointSite {
	var result []*BreakpointSite
	for _, s := range c.sites {
		if s.isEnabled && !s.isHardware && s.InRange(low, high) {
			result = append(result, s)
		}
	}
	return result
}

func (c *breakpointSiteCollection) forEach(fn func(*BreakpointSite)) {
	for _, s := range c.sites {
		fn(s)
	}
}

func (c *breakpointSiteCollection) size() int {
	return len(c.sites)
}

func (c *breakpointSiteCollection) empty() bool {
	return len(c.sites) == 0
}

// nextBreakpointID returns the next unique breakpoint ID.
func nextBreakpointID() int32 {
	return atomic.AddInt32(&breakpointIDCounter, 1)
}
