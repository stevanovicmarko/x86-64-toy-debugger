package debugger

import "path/filepath"

// ELFCollection holds multiple ELF objects (main binary + shared libraries).
// It provides lookup by path, filename, or virtual address — enabling the
// debugger to resolve symbols and DWARF info across all loaded objects.
type ELFCollection struct {
	elves []*ELF
}

// Push adds an ELF object to the collection.
func (c *ELFCollection) Push(e *ELF) {
	c.elves = append(c.elves, e)
}

// ForEach calls f for every ELF in the collection.
func (c *ELFCollection) ForEach(f func(*ELF)) {
	for _, e := range c.elves {
		f(e)
	}
}

// ELFContainingAddress returns the ELF whose address range contains
// the given virtual address, or nil if none.
func (c *ELFCollection) ELFContainingAddress(addr uint64) *ELF {
	for _, e := range c.elves {
		if e.ContainsAddress(addr) {
			return e
		}
	}
	return nil
}

// ELFByPath returns the ELF with the given exact path, or nil.
func (c *ELFCollection) ELFByPath(path string) *ELF {
	for _, e := range c.elves {
		if e.path == path {
			return e
		}
	}
	return nil
}

// ELFByFilename returns the ELF whose filename (basename) matches, or nil.
func (c *ELFCollection) ELFByFilename(name string) *ELF {
	for _, e := range c.elves {
		if filepath.Base(e.path) == name {
			return e
		}
	}
	return nil
}

// Close closes all ELF files in the collection.
func (c *ELFCollection) Close() {
	for _, e := range c.elves {
		e.Close()
	}
	c.elves = nil
}
