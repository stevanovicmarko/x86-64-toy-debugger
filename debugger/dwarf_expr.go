package debugger

import (
	"encoding/binary"
	"fmt"
)

// ---------------------------------------------------------------------------
// DWARF Expression Evaluator
//
// DWARF uses a stack-based bytecode language to describe where variables
// live (registers, memory, or computed values). The DWARF spec (Section 2.5)
// defines ~100 opcodes that operate on a stack of uint64 values.
//
// This evaluator supports the full taxonomy of DWARF expressions:
//
//   Single location description:
//     - Register:  variable is stored in a register (DW_OP_reg0..31, DW_OP_regx)
//     - Address:   variable is at a computed memory address (stack result)
//     - Implicit:  variable has no storage but a known value (DW_OP_stack_value, DW_OP_implicit_value)
//     - Empty:     variable location is unknown (zero-length expression)
//
//   Composite location description:
//     - A variable split across multiple locations (DW_OP_piece, DW_OP_bit_piece)
//
//   Location list:
//     - Variable whose location changes depending on the program counter
//     - Stored in .debug_loc section, indexed by DW_FORM_sec_offset
// ---------------------------------------------------------------------------

// DWARF expression opcodes (from DWARF 4 spec, Section 7.7.1).
const (
	opAddr       = 0x03
	opDeref      = 0x06
	opConst1u    = 0x08
	opConst1s    = 0x09
	opConst2u    = 0x0a
	opConst2s    = 0x0b
	opConst4u    = 0x0c
	opConst4s    = 0x0d
	opConst8u    = 0x0e
	opConst8s    = 0x0f
	opConstu     = 0x10
	opConsts     = 0x11
	opDup        = 0x12
	opDrop       = 0x13
	opOver       = 0x14
	opPick       = 0x15
	opSwap       = 0x16
	opRot        = 0x17
	opXderef     = 0x18
	opAbs        = 0x19
	opAnd        = 0x1a
	opDiv        = 0x1b
	opMinus      = 0x1c
	opMod        = 0x1d
	opMul        = 0x1e
	opNeg        = 0x1f
	opNot        = 0x20
	opOr         = 0x21
	opPlus       = 0x22
	opPlusUconst = 0x23
	opShl        = 0x24
	opShr        = 0x25
	opShra       = 0x26
	opXor        = 0x27
	opSkip       = 0x2f
	opBra        = 0x28
	opEq         = 0x29
	opGe         = 0x2a
	opGt         = 0x2b
	opLe         = 0x2c
	opLt         = 0x2d
	opNe         = 0x2e
	opLit0       = 0x30 // through opLit31 = 0x4f
	opLit31      = 0x4f
	opReg0       = 0x50 // through opReg31 = 0x6f
	opReg31      = 0x6f
	opBreg0      = 0x70 // through opBreg31 = 0x8f
	opBreg31     = 0x8f
	opRegx       = 0x90
	opFbreg      = 0x91
	opBregx      = 0x92
	opPiece      = 0x93
	opDerefSize  = 0x94
	opXderefSize = 0x95
	opNop        = 0x96

	opPushObjAddr  = 0x97
	opCall2        = 0x98
	opCall4        = 0x99
	opCallRef      = 0x9a
	opFormTLSAddr  = 0x9b
	opCallFrameCFA = 0x9c
	opBitPiece     = 0x9d
	opImplicitVal  = 0x9e
	opStackValue   = 0x9f
)

// ---------------------------------------------------------------------------
// Result Types
//
// A DWARF expression evaluation produces one of several location types.
// These are Go's equivalent of the C++ std::variant approach from the book.
// ---------------------------------------------------------------------------

// LocationKind identifies what kind of location a DWARF expression computed.
type LocationKind int

const (
	LocEmpty    LocationKind = iota // no location — variable is optimized out
	LocAddress                      // variable lives at a memory address
	LocRegister                     // variable lives in a register
	LocLiteral                      // variable value is on the stack (DW_OP_stack_value)
	LocData                         // variable value is inline data (DW_OP_implicit_value)
)

// SimpleLocation describes where a single contiguous piece of data lives.
type SimpleLocation struct {
	Kind    LocationKind
	Address uint64 // valid when Kind == LocAddress
	RegNum  uint64 // valid when Kind == LocRegister (DWARF register ID)
	Value   uint64 // valid when Kind == LocLiteral
	Data    []byte // valid when Kind == LocData
}

// Piece describes one fragment of a composite location.
type Piece struct {
	Location SimpleLocation
	BitSize  uint64
	Offset   uint64 // bit offset within this piece's location
}

// ExprResult holds the full result of evaluating a DWARF expression.
// Either it's a single SimpleLocation, or it's a composite of Pieces.
type ExprResult struct {
	IsComposite bool
	Location    SimpleLocation // valid when !IsComposite
	Pieces      []Piece        // valid when IsComposite
}

// ---------------------------------------------------------------------------
// DwarfExpression — the evaluator
// ---------------------------------------------------------------------------

// DwarfExpression evaluates a DWARF location expression bytecode program.
// It operates on a stack of uint64 values, reading registers and memory
// from the traced process as needed.
type DwarfExpression struct {
	data        []byte   // raw expression bytecode
	inFrameInfo bool     // true if expression is in .eh_frame / DW_AT_frame_base context
	loadBias    uint64   // for converting file addresses to virtual addresses
	dwarfData   *DWARF   // parent DWARF info (for DW_OP_fbreg lookups)
}

// NewDwarfExpression creates an evaluator for the given expression bytecode.
func NewDwarfExpression(data []byte, inFrameInfo bool, loadBias uint64, dw *DWARF) *DwarfExpression {
	return &DwarfExpression{
		data:        data,
		inFrameInfo: inFrameInfo,
		loadBias:    loadBias,
		dwarfData:   dw,
	}
}

// Eval executes the DWARF expression bytecode and returns the result.
// proc and regs provide access to the tracee's memory and register state.
// If pushCFA is true, the CFA value is pushed onto the stack before
// execution begins (used by CFI expression and val_expression rules).
func (e *DwarfExpression) Eval(proc *Process, regs *Registers, pushCFA bool, cfa uint64) (ExprResult, error) {
	if len(e.data) == 0 {
		return ExprResult{Location: SimpleLocation{Kind: LocEmpty}}, nil
	}

	reader := &cfiReader{data: e.data}
	var stack []uint64

	if pushCFA {
		stack = append(stack, cfa)
	}

	var mostRecentLoc *SimpleLocation
	var pieces []Piece
	resultIsAddress := true

	// Lambda-style helpers as closures.
	binop := func(op func(uint64, uint64) uint64) error {
		if len(stack) < 2 {
			return fmt.Errorf("DWARF expr: stack underflow in binary operation")
		}
		rhs := stack[len(stack)-1]
		lhs := stack[len(stack)-2]
		stack = stack[:len(stack)-2]
		stack = append(stack, op(lhs, rhs))
		return nil
	}

	relop := func(op func(int64, int64) bool) error {
		if len(stack) < 2 {
			return fmt.Errorf("DWARF expr: stack underflow in relational operation")
		}
		rhs := int64(stack[len(stack)-1])
		lhs := int64(stack[len(stack)-2])
		stack = stack[:len(stack)-2]
		if op(lhs, rhs) {
			stack = append(stack, 1)
		} else {
			stack = append(stack, 0)
		}
		return nil
	}

	getCurrentLocation := func() SimpleLocation {
		if len(stack) == 0 {
			if mostRecentLoc != nil {
				loc := *mostRecentLoc
				mostRecentLoc = nil
				return loc
			}
			return SimpleLocation{Kind: LocEmpty}
		}
		if resultIsAddress {
			loc := SimpleLocation{Kind: LocAddress, Address: stack[len(stack)-1]}
			stack = stack[:len(stack)-1]
			return loc
		}
		loc := SimpleLocation{Kind: LocLiteral, Value: stack[len(stack)-1]}
		stack = stack[:len(stack)-1]
		resultIsAddress = true
		return loc
	}

	// Read the RIP register for function lookups.
	ripInfo, _ := RegisterInfoByName("rip")

	for reader.remaining() > 0 {
		opByte, err := reader.readU8()
		if err != nil {
			return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
		}

		// Handle opcode ranges first.
		switch {
		case opByte >= opLit0 && opByte <= opLit31:
			stack = append(stack, uint64(opByte-opLit0))
			continue

		case opByte >= opBreg0 && opByte <= opBreg31:
			dwarfReg := int(opByte - opBreg0)
			regInfo, ok := RegisterInfoByDwarf(dwarfReg)
			if !ok {
				return ExprResult{}, fmt.Errorf("DWARF expr: unknown DWARF register %d", dwarfReg)
			}
			regVal := regs.Read(regInfo).(uint64)
			offset, err := reader.readSLEB128()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			stack = append(stack, uint64(int64(regVal)+offset))
			continue

		case opByte >= opReg0 && opByte <= opReg31:
			dwarfReg := uint64(opByte - opReg0)
			if e.inFrameInfo {
				regInfo, ok := RegisterInfoByDwarf(int(dwarfReg))
				if !ok {
					return ExprResult{}, fmt.Errorf("DWARF expr: unknown DWARF register %d", dwarfReg)
				}
				regVal := regs.Read(regInfo).(uint64)
				stack = append(stack, regVal)
			} else {
				loc := SimpleLocation{Kind: LocRegister, RegNum: dwarfReg}
				mostRecentLoc = &loc
			}
			continue
		}

		// Handle individual opcodes.
		switch opByte {
		case opAddr:
			fileAddr, err := reader.readU64()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			stack = append(stack, fileAddr+e.loadBias)

		case opConst1u:
			v, err := reader.readU8()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			stack = append(stack, uint64(v))

		case opConst1s:
			v, err := reader.readU8()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			stack = append(stack, uint64(int64(int8(v))))

		case opConst2u:
			v, err := reader.readU16()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			stack = append(stack, uint64(v))

		case opConst2s:
			v, err := reader.readU16()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			stack = append(stack, uint64(int64(int16(v))))

		case opConst4u:
			v, err := reader.readU32()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			stack = append(stack, uint64(v))

		case opConst4s:
			v, err := reader.readU32()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			stack = append(stack, uint64(int64(int32(v))))

		case opConst8u:
			v, err := reader.readU64()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			stack = append(stack, v)

		case opConst8s:
			v, err := reader.readU64()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			stack = append(stack, v) // same bit pattern

		case opConstu:
			v, err := reader.readULEB128()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			stack = append(stack, v)

		case opConsts:
			v, err := reader.readSLEB128()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			stack = append(stack, uint64(v))

		case opBregx:
			dwarfReg, err := reader.readULEB128()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			regInfo, ok := RegisterInfoByDwarf(int(dwarfReg))
			if !ok {
				return ExprResult{}, fmt.Errorf("DWARF expr: unknown DWARF register %d", dwarfReg)
			}
			regVal := regs.Read(regInfo).(uint64)
			offset, err := reader.readSLEB128()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			stack = append(stack, uint64(int64(regVal)+offset))

		case opFbreg:
			offset, err := reader.readSLEB128()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			// Evaluate the DW_AT_frame_base expression to find the frame base address.
			fbAddr, err := e.evalFrameBase(proc, regs)
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr DW_OP_fbreg: %w", err)
			}
			stack = append(stack, uint64(int64(fbAddr)+offset))

		// Stack operations.
		case opDup:
			if len(stack) < 1 {
				return ExprResult{}, fmt.Errorf("DWARF expr: stack underflow in DW_OP_dup")
			}
			stack = append(stack, stack[len(stack)-1])

		case opDrop:
			if len(stack) < 1 {
				return ExprResult{}, fmt.Errorf("DWARF expr: stack underflow in DW_OP_drop")
			}
			stack = stack[:len(stack)-1]

		case opPick:
			idx, err := reader.readU8()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			if int(idx) >= len(stack) {
				return ExprResult{}, fmt.Errorf("DWARF expr: DW_OP_pick index %d out of range", idx)
			}
			stack = append(stack, stack[len(stack)-1-int(idx)])

		case opOver:
			if len(stack) < 2 {
				return ExprResult{}, fmt.Errorf("DWARF expr: stack underflow in DW_OP_over")
			}
			stack = append(stack, stack[len(stack)-2])

		case opSwap:
			if len(stack) < 2 {
				return ExprResult{}, fmt.Errorf("DWARF expr: stack underflow in DW_OP_swap")
			}
			n := len(stack)
			stack[n-1], stack[n-2] = stack[n-2], stack[n-1]

		case opRot:
			if len(stack) < 3 {
				return ExprResult{}, fmt.Errorf("DWARF expr: stack underflow in DW_OP_rot")
			}
			n := len(stack)
			// Rotate top three: [2] [1] [0] → [0] [2] [1]
			stack[n-1], stack[n-2], stack[n-3] = stack[n-2], stack[n-3], stack[n-1]

		// Dereference operations.
		case opDeref:
			if len(stack) < 1 {
				return ExprResult{}, fmt.Errorf("DWARF expr: stack underflow in DW_OP_deref")
			}
			addr := stack[len(stack)-1]
			data, err := proc.ReadMemory(addr, 8)
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr DW_OP_deref at 0x%x: %w", addr, err)
			}
			stack[len(stack)-1] = binary.LittleEndian.Uint64(data)

		case opDerefSize:
			if len(stack) < 1 {
				return ExprResult{}, fmt.Errorf("DWARF expr: stack underflow in DW_OP_deref_size")
			}
			size, err := reader.readU8()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			addr := stack[len(stack)-1]
			data, err := proc.ReadMemory(addr, int(size))
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr DW_OP_deref_size at 0x%x: %w", addr, err)
			}
			var val uint64
			for i, b := range data {
				val |= uint64(b) << (uint(i) * 8)
			}
			stack[len(stack)-1] = val

		case opXderef:
			return ExprResult{}, fmt.Errorf("DWARF expr: DW_OP_xderef not supported")

		case opXderefSize:
			return ExprResult{}, fmt.Errorf("DWARF expr: DW_OP_xderef_size not supported")

		// Entity-pushing opcodes.
		case opPushObjAddr:
			return ExprResult{}, fmt.Errorf("DWARF expr: DW_OP_push_object_address not supported")

		case opFormTLSAddr:
			return ExprResult{}, fmt.Errorf("DWARF expr: DW_OP_form_tls_address not supported")

		case opCallFrameCFA:
			stack = append(stack, cfa)

		// Arithmetic operations.
		case opPlus:
			if err := binop(func(a, b uint64) uint64 { return a + b }); err != nil {
				return ExprResult{}, err
			}

		case opMinus:
			if err := binop(func(a, b uint64) uint64 { return a - b }); err != nil {
				return ExprResult{}, err
			}

		case opMul:
			if err := binop(func(a, b uint64) uint64 { return a * b }); err != nil {
				return ExprResult{}, err
			}

		case opMod:
			if err := binop(func(a, b uint64) uint64 {
				if b == 0 {
					return 0
				}
				return a % b
			}); err != nil {
				return ExprResult{}, err
			}

		case opAnd:
			if err := binop(func(a, b uint64) uint64 { return a & b }); err != nil {
				return ExprResult{}, err
			}

		case opOr:
			if err := binop(func(a, b uint64) uint64 { return a | b }); err != nil {
				return ExprResult{}, err
			}

		case opXor:
			if err := binop(func(a, b uint64) uint64 { return a ^ b }); err != nil {
				return ExprResult{}, err
			}

		case opShl:
			if err := binop(func(a, b uint64) uint64 { return a << b }); err != nil {
				return ExprResult{}, err
			}

		case opShr:
			if err := binop(func(a, b uint64) uint64 { return a >> b }); err != nil {
				return ExprResult{}, err
			}

		case opShra:
			// Arithmetic right shift: sign-extend the high bit.
			if err := binop(func(a, b uint64) uint64 {
				return uint64(int64(a) >> b)
			}); err != nil {
				return ExprResult{}, err
			}

		case opDiv:
			// Division interprets operands as signed.
			if len(stack) < 2 {
				return ExprResult{}, fmt.Errorf("DWARF expr: stack underflow in DW_OP_div")
			}
			rhs := int64(stack[len(stack)-1])
			lhs := int64(stack[len(stack)-2])
			stack = stack[:len(stack)-2]
			if rhs == 0 {
				return ExprResult{}, fmt.Errorf("DWARF expr: division by zero")
			}
			stack = append(stack, uint64(lhs/rhs))

		case opAbs:
			if len(stack) < 1 {
				return ExprResult{}, fmt.Errorf("DWARF expr: stack underflow in DW_OP_abs")
			}
			v := int64(stack[len(stack)-1])
			if v < 0 {
				v = -v
			}
			stack[len(stack)-1] = uint64(v)

		case opNeg:
			if len(stack) < 1 {
				return ExprResult{}, fmt.Errorf("DWARF expr: stack underflow in DW_OP_neg")
			}
			stack[len(stack)-1] = uint64(-int64(stack[len(stack)-1]))

		case opNot:
			if len(stack) < 1 {
				return ExprResult{}, fmt.Errorf("DWARF expr: stack underflow in DW_OP_not")
			}
			stack[len(stack)-1] = ^stack[len(stack)-1]

		case opPlusUconst:
			v, err := reader.readULEB128()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			if len(stack) < 1 {
				return ExprResult{}, fmt.Errorf("DWARF expr: stack underflow in DW_OP_plus_uconst")
			}
			stack[len(stack)-1] += v

		// Relational operations (operands are treated as signed).
		case opEq:
			if err := relop(func(a, b int64) bool { return a == b }); err != nil {
				return ExprResult{}, err
			}
		case opNe:
			if err := relop(func(a, b int64) bool { return a != b }); err != nil {
				return ExprResult{}, err
			}
		case opLt:
			if err := relop(func(a, b int64) bool { return a < b }); err != nil {
				return ExprResult{}, err
			}
		case opLe:
			if err := relop(func(a, b int64) bool { return a <= b }); err != nil {
				return ExprResult{}, err
			}
		case opGt:
			if err := relop(func(a, b int64) bool { return a > b }); err != nil {
				return ExprResult{}, err
			}
		case opGe:
			if err := relop(func(a, b int64) bool { return a >= b }); err != nil {
				return ExprResult{}, err
			}

		// Control flow.
		case opSkip:
			offset, err := reader.readS16()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			reader.pos += int(offset)

		case opBra:
			offset, err := reader.readS16()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			if len(stack) < 1 {
				return ExprResult{}, fmt.Errorf("DWARF expr: stack underflow in DW_OP_bra")
			}
			val := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if val != 0 {
				reader.pos += int(offset)
			}

		case opCall2:
			return ExprResult{}, fmt.Errorf("DWARF expr: DW_OP_call2 not supported")
		case opCall4:
			return ExprResult{}, fmt.Errorf("DWARF expr: DW_OP_call4 not supported")
		case opCallRef:
			return ExprResult{}, fmt.Errorf("DWARF expr: DW_OP_call_ref not supported")

		// Non-address location types.
		case opRegx:
			dwarfReg, err := reader.readULEB128()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			if e.inFrameInfo {
				regInfo, ok := RegisterInfoByDwarf(int(dwarfReg))
				if !ok {
					return ExprResult{}, fmt.Errorf("DWARF expr: unknown DWARF register %d", dwarfReg)
				}
				regVal := regs.Read(regInfo).(uint64)
				stack = append(stack, regVal)
			} else {
				loc := SimpleLocation{Kind: LocRegister, RegNum: dwarfReg}
				mostRecentLoc = &loc
			}

		case opImplicitVal:
			length, err := reader.readULEB128()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			if reader.pos+int(length) > len(reader.data) {
				return ExprResult{}, fmt.Errorf("DWARF expr: DW_OP_implicit_value data overflows")
			}
			data := make([]byte, length)
			copy(data, reader.data[reader.pos:reader.pos+int(length)])
			reader.pos += int(length)
			loc := SimpleLocation{Kind: LocData, Data: data}
			mostRecentLoc = &loc

		case opStackValue:
			resultIsAddress = false

		case opNop:
			// Do nothing.

		// Composite location descriptions.
		case opPiece:
			byteSize, err := reader.readULEB128()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			loc := getCurrentLocation()
			pieces = append(pieces, Piece{Location: loc, BitSize: byteSize * 8})

		case opBitPiece:
			bitSize, err := reader.readULEB128()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			bitOffset, err := reader.readULEB128()
			if err != nil {
				return ExprResult{}, fmt.Errorf("DWARF expr: %w", err)
			}
			loc := getCurrentLocation()
			pieces = append(pieces, Piece{Location: loc, BitSize: bitSize, Offset: bitOffset})

		default:
			return ExprResult{}, fmt.Errorf("DWARF expr: unknown opcode 0x%02x", opByte)
		}

		_ = ripInfo // suppress unused warning if not needed
	}

	if len(pieces) > 0 {
		return ExprResult{IsComposite: true, Pieces: pieces}, nil
	}
	return ExprResult{Location: getCurrentLocation()}, nil
}

// evalFrameBase evaluates the DW_AT_frame_base attribute of the function
// containing the current PC. This is used by DW_OP_fbreg to find the
// base address of the current stack frame.
func (e *DwarfExpression) evalFrameBase(proc *Process, regs *Registers) (uint64, error) {
	if e.dwarfData == nil {
		return 0, fmt.Errorf("no DWARF data available for frame base lookup")
	}

	ripInfo, _ := RegisterInfoByName("rip")
	pc := regs.Read(ripInfo).(uint64)

	fn := e.dwarfData.FunctionContainingPC(pc)
	if fn == nil {
		return 0, fmt.Errorf("no function found at PC 0x%x for frame base", pc)
	}

	// Read the DW_AT_frame_base attribute from the function's DIE.
	result, err := e.dwarfData.EvalLocationAttr(proc, regs, fn.DIEOffset, true)
	if err != nil {
		return 0, fmt.Errorf("evaluate frame base: %w", err)
	}

	if result.IsComposite {
		return 0, fmt.Errorf("frame base cannot be composite")
	}
	switch result.Location.Kind {
	case LocAddress:
		return result.Location.Address, nil
	case LocLiteral:
		return result.Location.Value, nil
	default:
		return 0, fmt.Errorf("unsupported frame base location kind: %d", result.Location.Kind)
	}
}

// readS16 reads a signed 16-bit value from the cfiReader (little-endian).
func (r *cfiReader) readS16() (int16, error) {
	v, err := r.readU16()
	if err != nil {
		return 0, err
	}
	return int16(v), nil
}
