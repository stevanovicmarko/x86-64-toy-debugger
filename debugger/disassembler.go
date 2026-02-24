package debugger

import (
	"fmt"

	"golang.org/x/arch/x86/x86asm"
)

// Instruction represents a single decoded machine instruction.
type Instruction struct {
	Address uint64 // Virtual address of the instruction
	Text    string // Human-readable disassembly (AT&T syntax)
}

// Disassembler decodes machine instructions from a traced process's memory.
type Disassembler struct {
	proc *Process
}

// NewDisassembler creates a Disassembler attached to the given process.
func NewDisassembler(proc *Process) *Disassembler {
	return &Disassembler{proc: proc}
}

// Disassemble decodes up to nInstructions starting at addr. If addr is
// nil, the current program counter is used. It returns the decoded
// instructions in program order.
func (d *Disassembler) Disassemble(nInstructions int, addr *uint64) ([]Instruction, error) {
	startAddr := uint64(0)
	if addr != nil {
		startAddr = *addr
	} else {
		pc, err := d.proc.GetPC()
		if err != nil {
			return nil, err
		}
		startAddr = pc
	}

	// Read enough bytes to decode nInstructions (max 15 bytes each on x86-64).
	const maxInsnLen = 15
	amount := nInstructions * maxInsnLen
	data, err := d.proc.ReadMemoryWithoutTraps(startAddr, amount)
	if err != nil {
		return nil, newErrorf("disassemble: read memory at 0x%x: %v", startAddr, err)
	}

	var instructions []Instruction
	offset := 0
	for i := 0; i < nInstructions && offset < len(data); i++ {
		inst, err := x86asm.Decode(data[offset:], 64)
		if err != nil {
			// If decoding fails, emit a raw byte as a data directive.
			instructions = append(instructions, Instruction{
				Address: startAddr + uint64(offset),
				Text:    fmt.Sprintf(".byte 0x%02x", data[offset]),
			})
			offset++
			continue
		}

		text := x86asm.GNUSyntax(inst, startAddr+uint64(offset), nil)
		instructions = append(instructions, Instruction{
			Address: startAddr + uint64(offset),
			Text:    text,
		})
		offset += inst.Len
	}

	return instructions, nil
}
