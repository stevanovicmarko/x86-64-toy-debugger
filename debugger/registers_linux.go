//go:build linux

package debugger

import (
	"encoding/binary"
	"math"
	"syscall"
	"unsafe"
)

// fpRegs mirrors the Linux user_fpregs_struct (512 bytes total).
//
//	struct user_fpregs_struct {
//	    uint16  cwd, swd, ftw, fop;     //  8 bytes
//	    uint64  rip, rdp;               // 16 bytes
//	    uint32  mxcsr, mxcr_mask;       //  8 bytes
//	    uint32  st_space[32];           // 128 bytes  (8 ST/MM regs × 16)
//	    uint32  xmm_space[64];          // 256 bytes  (16 XMM regs × 16)
//	    uint32  padding[24];            //  96 bytes
//	};                                  // total 512
type fpRegs struct {
	Cwd      uint16
	Swd      uint16
	Ftw      uint16
	Fop      uint16
	Rip      uint64
	Rdp      uint64
	Mxcsr    uint32
	MxcrMask uint32
	StSpace  [32]uint32
	XmmSpace [64]uint32
	Padding  [24]uint32
}

// Registers caches the full register state of a traced process.
// It is populated automatically by WaitOnSignal whenever the process
// stops, and individual registers can be read or written through it.
type Registers struct {
	pid   int
	gprs  syscall.PtraceRegs
	fprs  fpRegs
	dregs [8]uint64
}

// readAll populates the register cache by reading all GPRs, FPRs,
// and debug registers from the tracee via ptrace.
func (r *Registers) readAll() error {
	if err := ptraceGetRegs(r.pid, &r.gprs); err != nil {
		return newErrorf("could not read GPR registers: %v", err)
	}
	if err := ptraceGetFPRegs(r.pid, &r.fprs); err != nil {
		return newErrorf("could not read FPR registers: %v", err)
	}
	for i := 0; i < 8; i++ {
		off := uintptr(offDebugReg + i*8)
		val, err := ptracePeekUser(r.pid, off)
		if err != nil {
			return newErrorf("could not read debug register %d: %v", i, err)
		}
		r.dregs[i] = val
	}
	return nil
}

// bytesForOffset returns a byte-slice view into the backing store that
// contains the register at the given struct-user offset, along with the
// local offset within that slice.
func (r *Registers) bytesForOffset(off, size int) []byte {
	switch {
	case off < 216:
		base := (*[216]byte)(unsafe.Pointer(&r.gprs))
		return base[off : off+size]
	case off >= offFPR && off < offFPR+512:
		local := off - offFPR
		base := (*[512]byte)(unsafe.Pointer(&r.fprs))
		return base[local : local+size]
	default: // debug registers
		local := off - offDebugReg
		base := (*[64]byte)(unsafe.Pointer(&r.dregs))
		return base[local : local+size]
	}
}

// Read returns the value of a register as a typed Go value.
//
// The returned type depends on the register's format and size:
//
//	FormatUint:        uint8 | uint16 | uint32 | uint64
//	FormatDoubleFloat: float64
//	FormatLongDouble:  [16]byte  (raw 80-bit extended, stored in 16 bytes)
//	FormatVector:      [8]byte | [16]byte
func (r *Registers) Read(info RegisterInfo) any {
	raw := r.bytesForOffset(info.Offset, info.Size)

	switch info.Format {
	case RegisterFormatUint:
		switch info.Size {
		case 1:
			return raw[0]
		case 2:
			return binary.LittleEndian.Uint16(raw)
		case 4:
			return binary.LittleEndian.Uint32(raw)
		case 8:
			return binary.LittleEndian.Uint64(raw)
		}
	case RegisterFormatDoubleFloat:
		bits := binary.LittleEndian.Uint64(raw)
		return math.Float64frombits(bits)
	case RegisterFormatLongDouble:
		var out [16]byte
		copy(out[:], raw)
		return out
	case RegisterFormatVector:
		if info.Size == 8 {
			var out [8]byte
			copy(out[:], raw)
			return out
		}
		var out [16]byte
		copy(out[:], raw)
		return out
	}
	// Unreachable for valid register info.
	return nil
}

// ReadByID is a convenience wrapper that looks up a register by ID
// and reads its value.
func (r *Registers) ReadByID(id RegisterID) (any, error) {
	info, ok := RegisterInfoByID(id)
	if !ok {
		return nil, newErrorf("unknown register ID %d", id)
	}
	return r.Read(info), nil
}

// Write updates a register in the local cache and then syncs the
// change to the tracee via ptrace.
//
// Accepted value types mirror those returned by Read:
//
//	uint8, uint16, uint32, uint64, int8, int16, int32, int64,
//	float64, [8]byte, [16]byte
//
// Values smaller than the target register are widened: unsigned
// integers are zero-extended, signed integers are sign-extended,
// and floats are promoted.
func (r *Registers) Write(info RegisterInfo, val any) error {
	// Encode val into a 16-byte buffer, widened to info.Size.
	var buf [16]byte
	n := encodeValue(info, val, buf[:])
	if n < 0 {
		return newErrorf("cannot write %T to %d-byte register %s", val, info.Size, info.Name)
	}

	// Copy the encoded bytes into our local cache.
	dst := r.bytesForOffset(info.Offset, info.Size)
	copy(dst, buf[:info.Size])

	// Sync to the tracee.
	if info.Type == RegisterTypeFPR {
		// PTRACE_POKEUSER doesn't work for the FPR area; we must
		// write the entire floating-point state at once.
		return ptraceSetFPRegs(r.pid, &r.fprs)
	}

	// For GPRs, sub-GPRs, and debug registers we write a single
	// 8-byte-aligned word via PTRACE_POKEUSER.
	alignedOff := info.Offset &^ 7
	word := r.bytesForOffset(alignedOff, 8)
	return ptracePokeUser(r.pid, uintptr(alignedOff),
		binary.LittleEndian.Uint64(word))
}

// WriteByID is a convenience wrapper that looks up a register by ID
// and writes a value to it.
func (r *Registers) WriteByID(id RegisterID, val any) error {
	info, ok := RegisterInfoByID(id)
	if !ok {
		return newErrorf("unknown register ID %d", id)
	}
	return r.Write(info, val)
}

// WriteFPRs writes the entire floating-point register state to the tracee.
func (r *Registers) WriteFPRs() error {
	return ptraceSetFPRegs(r.pid, &r.fprs)
}

// WriteGPRs writes the entire general-purpose register state to the tracee.
func (r *Registers) WriteGPRs() error {
	return ptraceSetRegs(r.pid, &r.gprs)
}

// encodeValue converts val into bytes suitable for writing to a register
// described by info, widening as necessary. It writes into buf and returns
// the number of bytes written, or -1 on type mismatch.
func encodeValue(info RegisterInfo, val any, buf []byte) int {
	size := info.Size

	switch v := val.(type) {

	// ── Unsigned integers ───────────────────────────────────────
	case uint8:
		if size < 1 {
			return -1
		}
		binary.LittleEndian.PutUint64(buf, uint64(v))
		return size
	case uint16:
		if size < 2 {
			return -1
		}
		binary.LittleEndian.PutUint64(buf, uint64(v))
		return size
	case uint32:
		if size < 4 {
			return -1
		}
		binary.LittleEndian.PutUint64(buf, uint64(v))
		return size
	case uint64:
		if size < 8 {
			return -1
		}
		binary.LittleEndian.PutUint64(buf, v)
		return size

	// ── Signed integers (sign-extended) ─────────────────────────
	case int8:
		if size < 1 {
			return -1
		}
		// Sign-extend to 64 bits, then store.
		binary.LittleEndian.PutUint64(buf, uint64(int64(v)))
		return size
	case int16:
		if size < 2 {
			return -1
		}
		binary.LittleEndian.PutUint64(buf, uint64(int64(v)))
		return size
	case int32:
		if size < 4 {
			return -1
		}
		binary.LittleEndian.PutUint64(buf, uint64(int64(v)))
		return size
	case int64:
		if size < 8 {
			return -1
		}
		binary.LittleEndian.PutUint64(buf, uint64(v))
		return size

	// ── Floating point ──────────────────────────────────────────
	case float64:
		if info.Format == RegisterFormatDoubleFloat && size >= 8 {
			binary.LittleEndian.PutUint64(buf, math.Float64bits(v))
			return size
		}
		return -1

	// ── Byte vectors ────────────────────────────────────────────
	case [8]byte:
		if size < 8 {
			return -1
		}
		copy(buf, v[:])
		return size
	case [16]byte:
		if size < 16 {
			return -1
		}
		copy(buf, v[:])
		return size
	}

	return -1
}
