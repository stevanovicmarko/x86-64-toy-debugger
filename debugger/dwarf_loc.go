package debugger

import (
	"debug/dwarf"
	"encoding/binary"
	"fmt"
)

// ---------------------------------------------------------------------------
// Location Attribute Evaluation
//
// DWARF encodes variable locations in two ways:
//   - DW_FORM_exprloc: a single location expression (ULEB128 length + data)
//   - DW_FORM_sec_offset: an offset into .debug_loc for a location list
//
// Location lists map program counter ranges to different expressions,
// handling variables whose storage changes across their lifetime
// (e.g., a value that starts in a register and spills to the stack).
// ---------------------------------------------------------------------------

// EvalLocationAttr reads the DW_AT_location (or DW_AT_frame_base) attribute
// from the DIE at the given offset and evaluates it in the context of the
// given process and register state.
//
// inFrameInfo should be true when evaluating DW_AT_frame_base (which
// changes DW_OP_reg* semantics to push register values rather than
// recording register locations).
func (d *DWARF) EvalLocationAttr(proc *Process, regs *Registers, dieOffset dwarf.Offset, inFrameInfo bool) (ExprResult, error) {
	reader := d.data.Reader()
	reader.Seek(dieOffset)
	entry, err := reader.Next()
	if err != nil || entry == nil {
		return ExprResult{}, fmt.Errorf("cannot read DIE at offset %d", dieOffset)
	}

	// Determine which attribute to read.
	attrTag := dwarf.AttrLocation
	if inFrameInfo {
		attrTag = dwarf.AttrFrameBase
	}

	field := entry.AttrField(attrTag)
	if field == nil {
		return ExprResult{}, fmt.Errorf("DIE at offset %d has no %s attribute", dieOffset, attrTag)
	}

	return d.evalLocationFieldWithCFA(proc, regs, field, inFrameInfo, 0)
}

// EvalLocationAttrWithCFA is like EvalLocationAttr but passes a pre-computed
// CFA to the expression evaluator. This is needed when the expression
// contains DW_OP_call_frame_cfa (common in DW_AT_frame_base attributes).
func (d *DWARF) EvalLocationAttrWithCFA(proc *Process, regs *Registers, dieOffset dwarf.Offset, inFrameInfo bool, cfa uint64) (ExprResult, error) {
	reader := d.data.Reader()
	reader.Seek(dieOffset)
	entry, err := reader.Next()
	if err != nil || entry == nil {
		return ExprResult{}, fmt.Errorf("cannot read DIE at offset %d", dieOffset)
	}

	attrTag := dwarf.AttrLocation
	if inFrameInfo {
		attrTag = dwarf.AttrFrameBase
	}

	field := entry.AttrField(attrTag)
	if field == nil {
		return ExprResult{}, fmt.Errorf("DIE at offset %d has no %s attribute", dieOffset, attrTag)
	}

	return d.evalLocationFieldWithCFA(proc, regs, field, inFrameInfo, cfa)
}

// evalLocationFieldWithCFA evaluates a location attribute field, dispatching
// between single expressions (exprloc) and location lists (sec_offset).
// The cfa parameter is passed to the expression evaluator so that
// DW_OP_call_frame_cfa can push the correct canonical frame address.
func (d *DWARF) evalLocationFieldWithCFA(proc *Process, regs *Registers, field *dwarf.Field, inFrameInfo bool, cfa uint64) (ExprResult, error) {
	switch field.Class {
	case dwarf.ClassExprLoc:
		// Single location expression — the value is a []byte of bytecode.
		data, ok := field.Val.([]byte)
		if !ok {
			return ExprResult{}, fmt.Errorf("DW_FORM_exprloc value is not []byte")
		}
		expr := NewDwarfExpression(data, inFrameInfo, d.loadBias, d)
		return expr.Eval(proc, regs, false, cfa)

	case dwarf.ClassLocListPtr:
		// Location list — the value is an offset into .debug_loc.
		offset, ok := field.Val.(int64)
		if !ok {
			return ExprResult{}, fmt.Errorf("DW_FORM_sec_offset value is not int64")
		}
		return d.evalLocationList(proc, regs, uint64(offset), inFrameInfo)

	default:
		return ExprResult{}, fmt.Errorf("unsupported location attribute class: %s", field.Class)
	}
}

// evalLocationList reads and evaluates a DWARF location list from the
// .debug_loc section. It finds the entry whose PC range contains the
// current program counter, then evaluates that entry's expression.
func (d *DWARF) evalLocationList(proc *Process, regs *Registers, locOffset uint64, inFrameInfo bool) (ExprResult, error) {
	// Read the .debug_loc section data from the underlying ELF.
	// We need access to the raw section data, which Go's debug/dwarf
	// doesn't expose directly. We'll use a helper on ELF for this.
	locData, err := d.getDebugLocData()
	if err != nil {
		return ExprResult{}, fmt.Errorf("read .debug_loc: %w", err)
	}

	if int(locOffset) >= len(locData) {
		return ExprResult{}, fmt.Errorf(".debug_loc offset %d out of range (section size %d)", locOffset, len(locData))
	}

	// Get the current PC as a file address.
	ripInfo, _ := RegisterInfoByName("rip")
	virtPC := regs.Read(ripInfo).(uint64)
	filePC := virtPC - d.loadBias

	// Find the base address for this location list.
	// The initial base address comes from the compile unit's DW_AT_low_pc.
	baseAddr := d.getCompileUnitBaseAddress(filePC)

	reader := &cfiReader{data: locData[locOffset:]}

	for {
		first, err := reader.readU64()
		if err != nil {
			return ExprResult{}, fmt.Errorf("read .debug_loc entry: %w", err)
		}
		second, err := reader.readU64()
		if err != nil {
			return ExprResult{}, fmt.Errorf("read .debug_loc entry: %w", err)
		}

		// End-of-list entry.
		if first == 0 && second == 0 {
			break
		}

		// Base address selection entry.
		if first == ^uint64(0) {
			baseAddr = second
			continue
		}

		// Location entry.
		length, err := reader.readU16()
		if err != nil {
			return ExprResult{}, fmt.Errorf("read .debug_loc expression length: %w", err)
		}

		if filePC >= baseAddr+first && filePC < baseAddr+second {
			// This entry's range contains the current PC.
			exprData := make([]byte, length)
			copy(exprData, reader.data[reader.pos:reader.pos+int(length)])
			expr := NewDwarfExpression(exprData, inFrameInfo, d.loadBias, d)
			return expr.Eval(proc, regs, false, 0)
		}

		// Skip past this entry's expression data.
		reader.pos += int(length)
	}

	// No matching entry — return empty result.
	return ExprResult{Location: SimpleLocation{Kind: LocEmpty}}, nil
}

// getDebugLocData returns the raw .debug_loc section data.
// This is cached on first access.
func (d *DWARF) getDebugLocData() ([]byte, error) {
	if d.debugLocData != nil {
		return d.debugLocData, nil
	}
	if d.elfFile == nil {
		return nil, fmt.Errorf(".debug_loc not available (no ELF file)")
	}
	sec := d.elfFile.Section(".debug_loc")
	if sec == nil {
		return nil, fmt.Errorf(".debug_loc section not found")
	}
	data, err := sec.Data()
	if err != nil {
		return nil, err
	}
	d.debugLocData = data
	return data, nil
}

// getCompileUnitBaseAddress finds the DW_AT_low_pc of the compile unit
// containing the given file address. This is used as the initial base
// address for location list entries.
func (d *DWARF) getCompileUnitBaseAddress(filePC uint64) uint64 {
	reader := d.data.Reader()
	for {
		entry, err := reader.Next()
		if err != nil || entry == nil {
			break
		}
		if entry.Tag != dwarf.TagCompileUnit {
			reader.SkipChildren()
			continue
		}

		// Check if the PC falls within this CU's ranges.
		ranges, err := d.data.Ranges(entry)
		if err == nil {
			for _, r := range ranges {
				if filePC >= r[0] && filePC < r[1] {
					if lowPC, ok := entry.Val(dwarf.AttrLowpc).(uint64); ok {
						return lowPC
					}
					return 0
				}
			}
		}

		reader.SkipChildren()
	}
	return 0
}

// ---------------------------------------------------------------------------
// Global Variable Index
//
// The DWARF debug info contains DW_TAG_variable DIEs at compile-unit
// scope that represent global variables. We index them by name for
// quick lookup, mirroring the function index in dwarf.go.
// ---------------------------------------------------------------------------

// globalVarEntry stores the location of a global variable's DIE.
type globalVarEntry struct {
	name      string
	dieOffset dwarf.Offset
}

// FindGlobalVariable returns the DIE offset and name for a global
// variable with the given name, searching across all compile units.
func (d *DWARF) FindGlobalVariable(name string) (dwarf.Offset, bool) {
	d.buildGlobalVarIndex()

	for _, entry := range d.globalVars {
		if entry.name == name {
			return entry.dieOffset, true
		}
	}
	return 0, false
}

// buildGlobalVarIndex lazily builds the global variable index by
// walking the DIE tree once.
func (d *DWARF) buildGlobalVarIndex() {
	if d.globalVarsBuilt {
		return
	}
	d.globalVarsBuilt = true

	reader := d.data.Reader()
	inFunction := false
	functionDepth := 0

	for {
		entry, err := reader.Next()
		if err != nil || entry == nil {
			break
		}

		if entry.Tag == 0 {
			// End of children. Track function depth.
			if inFunction {
				functionDepth--
				if functionDepth == 0 {
					inFunction = false
				}
			}
			continue
		}

		switch entry.Tag {
		case dwarf.TagSubprogram:
			inFunction = true
			functionDepth = 1

		case dwarf.TagVariable:
			if !inFunction {
				// This is a global variable (not nested inside a function).
				name := entryName(entry)
				if name != "" && entry.AttrField(dwarf.AttrLocation) != nil {
					d.globalVars = append(d.globalVars, globalVarEntry{
						name:      name,
						dieOffset: entry.Offset,
					})
				}
			}
		}

		// Track nesting depth inside functions.
		if inFunction && entry.Children {
			functionDepth++
		}
	}
}

// ---------------------------------------------------------------------------
// Reading Location Data
//
// Given an ExprResult, read the actual bytes of the variable's value.
// This handles all location kinds: register, memory, literal, data,
// and composite.
// ---------------------------------------------------------------------------

// ReadLocationData reads the bytes of a variable from the location
// described by the given ExprResult. size is the number of bytes to read.
func ReadLocationData(proc *Process, regs *Registers, result ExprResult, size int) ([]byte, error) {
	if !result.IsComposite {
		return readSimpleLocationData(proc, regs, result.Location, size)
	}

	// Composite: assemble data from multiple pieces.
	data := make([]byte, size)
	bitOffset := uint64(0)

	for _, piece := range result.Pieces {
		byteSize := (piece.BitSize + 7) / 8
		pieceData, err := readSimpleLocationData(proc, regs, piece.Location, int(byteSize))
		if err != nil {
			return nil, fmt.Errorf("read piece: %w", err)
		}

		if bitOffset%8 == 0 && piece.Offset == 0 && piece.BitSize%8 == 0 {
			// Byte-aligned case: simple copy.
			byteOff := bitOffset / 8
			copy(data[byteOff:], pieceData)
		} else {
			// Bit-level copy for non-aligned pieces.
			memcpyBits(data, bitOffset, pieceData, piece.Offset, piece.BitSize)
		}
		bitOffset += piece.BitSize
	}

	return data, nil
}

// readSimpleLocationData reads bytes from a single (non-composite) location.
func readSimpleLocationData(proc *Process, regs *Registers, loc SimpleLocation, size int) ([]byte, error) {
	switch loc.Kind {
	case LocRegister:
		regInfo, ok := RegisterInfoByDwarf(int(loc.RegNum))
		if !ok {
			return nil, fmt.Errorf("unknown DWARF register %d", loc.RegNum)
		}
		val := regs.Read(regInfo)
		return registerValueToBytes(val, regInfo.Size), nil

	case LocAddress:
		return proc.ReadMemory(loc.Address, size)

	case LocLiteral:
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint64(buf, loc.Value)
		if size < 8 {
			return buf[:size], nil
		}
		return buf, nil

	case LocData:
		result := make([]byte, size)
		copy(result, loc.Data)
		return result, nil

	case LocEmpty:
		return nil, fmt.Errorf("variable location is unknown (optimized out)")

	default:
		return nil, fmt.Errorf("unsupported location kind: %d", loc.Kind)
	}
}

// registerValueToBytes converts a register value (as returned by
// Registers.Read) to a byte slice.
func registerValueToBytes(val any, regSize int) []byte {
	switch v := val.(type) {
	case uint64:
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint64(buf, v)
		if regSize < 8 {
			return buf[:regSize]
		}
		return buf
	case uint32:
		buf := make([]byte, 4)
		binary.LittleEndian.PutUint32(buf, v)
		return buf
	case uint16:
		buf := make([]byte, 2)
		binary.LittleEndian.PutUint16(buf, v)
		return buf
	case uint8:
		return []byte{v}
	case [8]byte:
		b := make([]byte, 8)
		copy(b, v[:])
		return b
	case [16]byte:
		b := make([]byte, 16)
		copy(b, v[:])
		return b
	default:
		// Fallback: try to extract as uint64.
		buf := make([]byte, regSize)
		return buf
	}
}

// memcpyBits copies n_bits bits from src (starting at srcBit) to dest
// (starting at destBit), one bit at a time.
func memcpyBits(dest []byte, destBit uint64, src []byte, srcBit uint64, nBits uint64) {
	for i := uint64(0); i < nBits; i++ {
		si := srcBit + i
		di := destBit + i

		// Clear the destination bit.
		destMask := byte(1 << (di % 8))
		dest[di/8] &^= destMask

		// Copy from source.
		srcMask := byte(1 << (si % 8))
		if si/8 < uint64(len(src)) && src[si/8]&srcMask != 0 {
			dest[di/8] |= destMask
		}
	}
}
