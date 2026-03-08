package debugger

import (
	"encoding/binary"
	"fmt"
)

// ---------------------------------------------------------------------------
// CFI Register Rules
//
// DWARF call frame information defines rules for how to restore each
// register to its caller's value. These rules are computed by executing
// CFI instructions (a bytecode program) that build up a row of the
// abstract unwind table.
// ---------------------------------------------------------------------------

// registerRuleKind identifies the type of a register restore rule.
type registerRuleKind int

const (
	ruleUndefined     registerRuleKind = iota
	ruleSameValue
	ruleOffset        // previous value at CFA + offset
	ruleValOffset     // previous value IS CFA + offset
	ruleRegister      // previous value stored in another register
	ruleExpression    // previous value at address computed by DWARF expression
	ruleValExpression // previous value IS the result of DWARF expression
)

// registerRule describes how to restore a single register.
type registerRule struct {
	kind     registerRuleKind
	offset   int64  // used by ruleOffset and ruleValOffset
	reg      uint64 // used by ruleRegister (DWARF register number)
	exprData []byte // used by ruleExpression and ruleValExpression
}

// cfaRule describes how to compute the Canonical Frame Address.
type cfaRule struct {
	reg      uint64 // DWARF register number
	offset   int64
	isExpr   bool   // true if CFA is computed by a DWARF expression
	exprData []byte // expression bytecode (when isExpr is true)
}

// ---------------------------------------------------------------------------
// Unwind Context — the abstract machine for CFI interpretation
//
// This mirrors the C++ unwind_context from the book. The CFI bytecode
// operates on this state, building up the correct register rules for
// a given program counter value.
// ---------------------------------------------------------------------------

type unwindContext struct {
	reader           *cfiReader
	location         uint64
	cfa              cfaRule
	registerRules    map[uint64]registerRule
	cieRegisterRules map[uint64]registerRule
	ruleStack        []savedRules
	loadBias         uint64 // needed by DWARF expression evaluation
}

type savedRules struct {
	registerRules map[uint64]registerRule
	cfa           cfaRule
}

func newUnwindContext() *unwindContext {
	return &unwindContext{
		registerRules:    make(map[uint64]registerRule),
		cieRegisterRules: make(map[uint64]registerRule),
	}
}

// copyRules makes a deep copy of a register rule map.
func copyRules(src map[uint64]registerRule) map[uint64]registerRule {
	dst := make(map[uint64]registerRule, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// ---------------------------------------------------------------------------
// DWARF CFI Opcodes
//
// The DWARF spec defines 25 CFI instructions. The high 2 bits of the
// first byte encode the "primary" opcode; if those bits are 0, the low
// 6 bits encode an "extended" opcode.
// ---------------------------------------------------------------------------

// Primary opcodes (high 2 bits).
const (
	dwCFAAdvanceLoc = 0x40 // delta in low 6 bits
	dwCFAOffset     = 0x80 // register in low 6 bits, ULEB128 offset follows
	dwCFARestore    = 0xC0 // register in low 6 bits
)

// Extended opcodes (low 6 bits when high 2 bits are 0).
// Values from DWARF 4 spec, Table 7.29.
const (
	dwCFANop              = 0x00
	dwCFASetLoc           = 0x01
	dwCFAAdvanceLoc1      = 0x02
	dwCFAAdvanceLoc2      = 0x03
	dwCFAAdvanceLoc4      = 0x04
	dwCFAOffsetExtended   = 0x05
	dwCFARestoreExtended  = 0x06
	dwCFAUndefined        = 0x07
	dwCFASameValue        = 0x08
	dwCFARegister         = 0x09
	dwCFARememberState    = 0x0A
	dwCFARestoreState     = 0x0B
	dwCFADefCFA           = 0x0C
	dwCFADefCFARegister   = 0x0D
	dwCFADefCFAOffset     = 0x0E
	dwCFADefCFAExpression = 0x0F
	dwCFAExpression       = 0x10
	dwCFAOffsetExtendedSF = 0x11
	dwCFADefCFASF         = 0x12
	dwCFADefCFAOffsetSF   = 0x13
	dwCFAValOffset        = 0x14
	dwCFAValOffsetSF      = 0x15
	dwCFAValExpression    = 0x16
)

// ---------------------------------------------------------------------------
// CFI Instruction Interpreter
// ---------------------------------------------------------------------------

// executeCFIInstruction reads and executes one CFI instruction from the
// context's reader, updating the unwind context accordingly.
func executeCFIInstruction(ctx *unwindContext, cie *CIE) error {
	opByte, err := ctx.reader.readU8()
	if err != nil {
		return err
	}

	primaryOpcode := opByte & 0xC0
	operand := opByte & 0x3F

	if primaryOpcode != 0 {
		switch primaryOpcode {
		case dwCFAAdvanceLoc:
			ctx.location += uint64(operand) * cie.CodeAlignmentFactor

		case dwCFAOffset:
			uleb, err := ctx.reader.readULEB128()
			if err != nil {
				return err
			}
			offset := int64(uleb) * cie.DataAlignmentFactor
			ctx.registerRules[uint64(operand)] = registerRule{kind: ruleOffset, offset: offset}

		case dwCFARestore:
			reg := uint64(operand)
			if rule, ok := ctx.cieRegisterRules[reg]; ok {
				ctx.registerRules[reg] = rule
			} else {
				ctx.registerRules[reg] = registerRule{kind: ruleUndefined}
			}
		}
		return nil
	}

	// Extended opcode.
	switch operand {
	case dwCFANop:
		// Do nothing.

	case dwCFASetLoc:
		loc, err := ctx.reader.readAddress(cie.FDEPointerEncoding)
		if err != nil {
			return err
		}
		ctx.location = loc

	case dwCFAAdvanceLoc1:
		v, err := ctx.reader.readU8()
		if err != nil {
			return err
		}
		ctx.location += uint64(v) * cie.CodeAlignmentFactor

	case dwCFAAdvanceLoc2:
		v, err := ctx.reader.readU16()
		if err != nil {
			return err
		}
		ctx.location += uint64(v) * cie.CodeAlignmentFactor

	case dwCFAAdvanceLoc4:
		v, err := ctx.reader.readU32()
		if err != nil {
			return err
		}
		ctx.location += uint64(v) * cie.CodeAlignmentFactor

	case dwCFADefCFA:
		reg, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		off, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		ctx.cfa = cfaRule{reg: reg, offset: int64(off)}

	case dwCFADefCFASF:
		reg, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		off, err := ctx.reader.readSLEB128()
		if err != nil {
			return err
		}
		ctx.cfa = cfaRule{reg: reg, offset: off * cie.DataAlignmentFactor}

	case dwCFADefCFARegister:
		reg, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		ctx.cfa.reg = reg

	case dwCFADefCFAOffset:
		off, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		ctx.cfa.offset = int64(off)

	case dwCFADefCFAOffsetSF:
		off, err := ctx.reader.readSLEB128()
		if err != nil {
			return err
		}
		ctx.cfa.offset = off * cie.DataAlignmentFactor

	case dwCFADefCFAExpression:
		length, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		sub, err := ctx.reader.slice(int(length))
		if err != nil {
			return err
		}
		exprData := make([]byte, len(sub.data))
		copy(exprData, sub.data)
		ctx.cfa = cfaRule{isExpr: true, exprData: exprData}

	case dwCFAUndefined:
		reg, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		ctx.registerRules[reg] = registerRule{kind: ruleUndefined}

	case dwCFASameValue:
		reg, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		ctx.registerRules[reg] = registerRule{kind: ruleSameValue}

	case dwCFAOffsetExtended:
		reg, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		uleb, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		offset := int64(uleb) * cie.DataAlignmentFactor
		ctx.registerRules[reg] = registerRule{kind: ruleOffset, offset: offset}

	case dwCFAOffsetExtendedSF:
		reg, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		off, err := ctx.reader.readSLEB128()
		if err != nil {
			return err
		}
		ctx.registerRules[reg] = registerRule{kind: ruleOffset, offset: off * cie.DataAlignmentFactor}

	case dwCFAValOffset:
		reg, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		uleb, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		offset := int64(uleb) * cie.DataAlignmentFactor
		ctx.registerRules[reg] = registerRule{kind: ruleValOffset, offset: offset}

	case dwCFAValOffsetSF:
		reg, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		off, err := ctx.reader.readSLEB128()
		if err != nil {
			return err
		}
		ctx.registerRules[reg] = registerRule{kind: ruleValOffset, offset: off * cie.DataAlignmentFactor}

	case dwCFARegister:
		reg, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		otherReg, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		ctx.registerRules[reg] = registerRule{kind: ruleRegister, reg: otherReg}

	case dwCFAExpression:
		reg, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		length, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		sub, err := ctx.reader.slice(int(length))
		if err != nil {
			return err
		}
		exprData := make([]byte, len(sub.data))
		copy(exprData, sub.data)
		ctx.registerRules[reg] = registerRule{kind: ruleExpression, exprData: exprData}

	case dwCFAValExpression:
		reg, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		length, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		sub, err := ctx.reader.slice(int(length))
		if err != nil {
			return err
		}
		exprData := make([]byte, len(sub.data))
		copy(exprData, sub.data)
		ctx.registerRules[reg] = registerRule{kind: ruleValExpression, exprData: exprData}

	case dwCFARestoreExtended:
		reg, err := ctx.reader.readULEB128()
		if err != nil {
			return err
		}
		if rule, ok := ctx.cieRegisterRules[reg]; ok {
			ctx.registerRules[reg] = rule
		} else {
			ctx.registerRules[reg] = registerRule{kind: ruleUndefined}
		}

	case dwCFARememberState:
		ctx.ruleStack = append(ctx.ruleStack, savedRules{
			registerRules: copyRules(ctx.registerRules),
			cfa:           ctx.cfa,
		})

	case dwCFARestoreState:
		if len(ctx.ruleStack) == 0 {
			return fmt.Errorf("DW_CFA_restore_state: rule stack is empty")
		}
		top := ctx.ruleStack[len(ctx.ruleStack)-1]
		ctx.ruleStack = ctx.ruleStack[:len(ctx.ruleStack)-1]
		ctx.registerRules = top.registerRules
		ctx.cfa = top.cfa

	default:
		return fmt.Errorf("unknown CFI extended opcode 0x%02x", operand)
	}

	return nil
}

// ---------------------------------------------------------------------------
// UnwindFrame — unwind one physical stack frame using CFI
//
// Given the current process, a virtual PC, and the current register state,
// this function finds the matching FDE, executes its CFI instructions to
// build the unwind table row for that PC, then applies the register
// restore rules to produce the caller's register state.
// ---------------------------------------------------------------------------

// UnwindFrame unwinds one stack frame at the given virtual address pc,
// using call frame information. It returns a new Registers snapshot
// representing the caller's register state and the computed CFA for
// the current frame. Returns an error if no CFI is available.
func (cfi *CallFrameInformation) UnwindFrame(proc *Process, pc uint64, regs *Registers) (*Registers, uint64, error) {
	// Find the FDE for this PC.
	fde, err := cfi.FDEForPC(pc)
	if err != nil {
		return nil, 0, fmt.Errorf("unwind: %w", err)
	}

	// Convert PC to file address for location comparison.
	filePC := pc - cfi.loadBias

	// Create the unwind context and execute CIE initial instructions.
	ctx := newUnwindContext()
	ctx.loadBias = cfi.loadBias
	if len(fde.CIE.Instructions) > 0 {
		ctx.reader = &cfiReader{
			data:     fde.CIE.Instructions,
			baseAddr: 0, // CIE instructions don't use pcrel addressing
		}
		for ctx.reader.remaining() > 0 {
			if err := executeCFIInstruction(ctx, fde.CIE); err != nil {
				return nil, 0, fmt.Errorf("unwind: execute CIE instruction: %w", err)
			}
		}
	}

	// Save the CIE register rules (needed for DW_CFA_restore).
	ctx.cieRegisterRules = copyRules(ctx.registerRules)

	// Execute FDE instructions until we pass the target PC.
	ctx.location = fde.InitialLocation
	if len(fde.Instructions) > 0 {
		ctx.reader = &cfiReader{
			data:     fde.Instructions,
			baseAddr: 0,
		}
		for ctx.reader.remaining() > 0 && ctx.location <= filePC {
			if err := executeCFIInstruction(ctx, fde.CIE); err != nil {
				return nil, 0, fmt.Errorf("unwind: execute FDE instruction: %w", err)
			}
		}
	}

	// Execute the unwind rules to produce the caller's registers.
	return executeUnwindRules(ctx, regs, proc)
}

// executeUnwindRules applies the computed CFA and register rules to
// produce a new register set representing the caller's state.
func executeUnwindRules(ctx *unwindContext, oldRegs *Registers, proc *Process) (*Registers, uint64, error) {
	// Make a copy of the registers for the caller frame.
	unwound := cloneRegisters(oldRegs)

	// Compute the CFA from the CFA rule.
	var cfa uint64
	if ctx.cfa.isExpr {
		// CFA is computed by a DWARF expression.
		expr := NewDwarfExpression(ctx.cfa.exprData, true, ctx.loadBias, nil)
		result, err := expr.Eval(proc, oldRegs, false, 0)
		if err != nil {
			return nil, 0, fmt.Errorf("unwind: evaluate CFA expression: %w", err)
		}
		if result.Location.Kind != LocAddress {
			return nil, 0, fmt.Errorf("unwind: CFA expression did not produce an address")
		}
		cfa = result.Location.Address
	} else {
		cfaRegInfo, ok := RegisterInfoByDwarf(int(ctx.cfa.reg))
		if !ok {
			return nil, 0, fmt.Errorf("unwind: unknown DWARF register %d for CFA", ctx.cfa.reg)
		}
		cfaRegVal := oldRegs.Read(cfaRegInfo).(uint64)
		cfa = cfaRegVal + uint64(ctx.cfa.offset)
	}

	// The CFA is the caller's stack pointer value.
	rspInfo, _ := RegisterInfoByName("rsp")
	writeRegisterValue(unwound, rspInfo, cfa)

	// Apply each register rule.
	for dwarfReg, rule := range ctx.registerRules {
		regInfo, ok := RegisterInfoByDwarf(int(dwarfReg))
		if !ok {
			continue // skip unknown registers
		}

		switch rule.kind {
		case ruleUndefined:
			// Register is undefined in the caller — we leave it as-is
			// but mark it conceptually undefined. For simplicity we
			// just don't touch it.

		case ruleSameValue:
			// Register hasn't changed — the value in oldRegs is correct.
			// Copy it to unwound (it's already there from the clone).

		case ruleRegister:
			otherInfo, ok := RegisterInfoByDwarf(int(rule.reg))
			if !ok {
				continue
			}
			val := oldRegs.Read(otherInfo).(uint64)
			writeRegisterValue(unwound, regInfo, val)

		case ruleOffset:
			// Previous value is stored in memory at CFA + offset.
			addr := uint64(int64(cfa) + rule.offset)
			data, err := proc.ReadMemory(addr, 8)
			if err != nil {
				return nil, 0, fmt.Errorf("unwind: read memory at 0x%x: %w", addr, err)
			}
			val := binary.LittleEndian.Uint64(data)
			writeRegisterValue(unwound, regInfo, val)

		case ruleValOffset:
			// Previous value IS CFA + offset (not stored in memory).
			val := uint64(int64(cfa) + rule.offset)
			writeRegisterValue(unwound, regInfo, val)

		case ruleExpression:
			// Previous value is at the address computed by a DWARF expression.
			// The CFA is pushed to the stack before evaluation.
			expr := NewDwarfExpression(rule.exprData, true, ctx.loadBias, nil)
			result, err := expr.Eval(proc, oldRegs, true, cfa)
			if err != nil {
				return nil, 0, fmt.Errorf("unwind: evaluate expression for register %d: %w", dwarfReg, err)
			}
			if result.Location.Kind != LocAddress {
				continue
			}
			data, err := proc.ReadMemory(result.Location.Address, 8)
			if err != nil {
				return nil, 0, fmt.Errorf("unwind: read memory at 0x%x for expression rule: %w", result.Location.Address, err)
			}
			val := binary.LittleEndian.Uint64(data)
			writeRegisterValue(unwound, regInfo, val)

		case ruleValExpression:
			// Previous value IS the result of the DWARF expression.
			// The CFA is pushed to the stack before evaluation.
			expr := NewDwarfExpression(rule.exprData, true, ctx.loadBias, nil)
			result, err := expr.Eval(proc, oldRegs, true, cfa)
			if err != nil {
				return nil, 0, fmt.Errorf("unwind: evaluate val_expression for register %d: %w", dwarfReg, err)
			}
			if result.Location.Kind != LocAddress {
				continue
			}
			writeRegisterValue(unwound, regInfo, result.Location.Address)
		}
	}

	return unwound, cfa, nil
}

// cloneRegisters creates a copy of a Registers struct. The clone is
// not attached to any process — writes to it won't affect the tracee.
func cloneRegisters(src *Registers) *Registers {
	dst := &Registers{
		pid: 0, // not attached to any process
	}
	dst.gprs = src.gprs
	dst.fprs = src.fprs
	dst.dregs = src.dregs
	return dst
}

// writeRegisterValue writes a uint64 value into a register's backing
// store without syncing to the tracee (pid=0 means no process).
func writeRegisterValue(regs *Registers, info RegisterInfo, val uint64) {
	raw := regs.bytesForOffset(info.Offset, 8)
	binary.LittleEndian.PutUint64(raw, val)
}

// readRegisterUint64 reads a uint64 from a register in a Registers snapshot.
func readRegisterUint64(regs *Registers, info RegisterInfo) uint64 {
	return regs.Read(info).(uint64)
}
