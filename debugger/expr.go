package debugger

import (
	"debug/dwarf"
	"debug/elf"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Expression Evaluation
//
// The expression evaluator allows the user to call functions inside the
// running process and inspect the results. It supports:
//
//   - Calling free functions: print_value(42)
//   - Passing literal arguments: integers, floats, strings, chars, bools
//   - Passing variable arguments: print_value(my_var)
//   - Referencing previous results: get_info($0)
//
// The evaluator works by:
//  1. Parsing the function name and arguments from user input
//  2. Locating the function via DWARF info or the ELF symbol table
//  3. Classifying arguments per the SysV x86-64 ABI
//  4. Setting up registers and stack with the arguments
//  5. Setting the program counter to the function start
//  6. Using the program entry point as the return address
//  7. Resuming until the return breakpoint fires
//  8. Reading the return value from registers or memory
// ---------------------------------------------------------------------------

// ExpressionResult holds the return value of an evaluated expression
// along with its sequential ID for later reference via $N syntax.
type ExpressionResult struct {
	Value *TypedData
	ID    int
}

// parameterClass represents the SysV x86-64 ABI classification for
// how function parameters are passed.
type parameterClass int

const (
	paramClassNoClass  parameterClass = iota // Padding or empty
	paramClassInteger                        // Passed in GPR (rdi, rsi, rdx, rcx, r8, r9)
	paramClassSSE                            // Passed in XMM register (xmm0-xmm7)
	paramClassMemory                         // Passed on the stack
	paramClassX87                            // Returned in st0 (long double)
	paramClassX87Up                          // Upper part of long double
)

// builtinArgType distinguishes literal argument types that don't come
// from DWARF (the user typed them on the command line).
type builtinArgType int

const (
	builtinNone   builtinArgType = iota // Not a builtin — from DWARF
	builtinInt                          // Integer literal (e.g., 42, -1)
	builtinFloat                        // Float literal (e.g., 3.14)
	builtinString                       // String literal (e.g., "hello")
	builtinChar                         // Char literal (e.g., 'a')
	builtinBool                         // Boolean literal (true/false)
)

// exprArg represents a parsed argument to a function call, holding
// both the raw bytes and type information needed for ABI classification.
type exprArg struct {
	data    []byte         // Raw argument bytes (native byte order)
	typ     dwarf.Type     // DWARF type (nil for builtin types)
	builtin builtinArgType // Non-zero if this is a literal from user input
	address *uint64        // Non-nil if the argument lives at a known address
}

// EvaluateExpression parses and evaluates a function call expression
// like "print_value(42)" or "get_name(target)". It calls the function
// in the inferior process and returns the result, if any.
func (t *Target) EvaluateExpression(expr string) (*ExpressionResult, error) {
	// Parse expression: find the opening parenthesis.
	parenPos := strings.Index(expr, "(")
	if parenPos < 0 {
		return nil, fmt.Errorf("invalid expression: missing '('")
	}

	funcName := strings.TrimSpace(expr[:parenPos])
	if funcName == "" {
		return nil, fmt.Errorf("invalid expression: empty function name")
	}

	// Parse argument string (everything between parens).
	closePos := strings.LastIndex(expr, ")")
	if closePos < 0 {
		return nil, fmt.Errorf("invalid expression: missing ')'")
	}
	argString := strings.TrimSpace(expr[parenPos+1 : closePos])

	// Locate the function to call.
	funcAddr, funcDIE, err := t.findCallableFunction(funcName)
	if err != nil {
		return nil, err
	}

	// Parse and collect arguments.
	args, err := t.parseArguments(argString)
	if err != nil {
		return nil, fmt.Errorf("parse arguments: %w", err)
	}

	// Get the entry point address for use as the return address.
	entryAddr, err := t.getEntryPointAddress()
	if err != nil {
		return nil, fmt.Errorf("get entry point: %w", err)
	}

	// Save current registers.
	proc := t.process
	regs := proc.Registers()
	if regs == nil {
		return nil, fmt.Errorf("registers not available")
	}
	savedRegs := *regs

	// Ensure we have a breakpoint at the entry point for the return.
	bp, bpExisted := proc.breakpointSites.getByAddress(entryAddr)
	if !bpExisted {
		var bpErr error
		bp, bpErr = proc.CreateBreakpointSite(entryAddr, false, true)
		if bpErr != nil {
			return nil, fmt.Errorf("create return breakpoint: %w", bpErr)
		}
		bp.Enable()
	}

	// Install a hit handler that stops (doesn't auto-resume).
	oldHandler := bp.onHit
	bp.InstallHitHandler(func() bool {
		return false // stop here
	})

	// Allocate storage for the return value if the function has one.
	var returnSlot *uint64
	var returnType dwarf.Type
	if funcDIE != nil {
		returnType = t.getFunctionReturnType(funcDIE)
		if returnType != nil {
			size := returnType.Size()
			if size <= 0 {
				size = 8
			}
			slot, err := t.inferiorMalloc(int(size))
			if err != nil {
				t.restoreExprState(bp, bpExisted, oldHandler, &savedRegs)
				return nil, fmt.Errorf("allocate return slot: %w", err)
			}
			returnSlot = &slot
		}
	}

	// Set up arguments according to the SysV ABI.
	err = t.setupArguments(args, funcDIE, regs, returnSlot, returnType)
	if err != nil {
		t.restoreExprState(bp, bpExisted, oldHandler, &savedRegs)
		return nil, fmt.Errorf("setup arguments: %w", err)
	}

	// Set RIP to the function address.
	ripInfo, _ := RegisterInfoByName("rip")
	if err := regs.Write(ripInfo, funcAddr); err != nil {
		t.restoreExprState(bp, bpExisted, oldHandler, &savedRegs)
		return nil, fmt.Errorf("set RIP: %w", err)
	}

	// Push the return address onto the stack.
	rspInfo, _ := RegisterInfoByName("rsp")
	rsp := regs.Read(rspInfo).(uint64)
	rsp -= 8
	retAddrBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(retAddrBytes, entryAddr)
	if err := proc.WriteMemory(rsp, retAddrBytes); err != nil {
		t.restoreExprState(bp, bpExisted, oldHandler, &savedRegs)
		return nil, fmt.Errorf("push return address: %w", err)
	}
	if err := regs.Write(rspInfo, rsp); err != nil {
		t.restoreExprState(bp, bpExisted, oldHandler, &savedRegs)
		return nil, fmt.Errorf("update RSP: %w", err)
	}

	// Resume and wait for the return breakpoint.
	if err := proc.Resume(); err != nil {
		t.restoreExprState(bp, bpExisted, oldHandler, &savedRegs)
		return nil, fmt.Errorf("resume for call: %w", err)
	}
	reason, err := proc.WaitOnSignal()
	if err != nil {
		t.restoreExprState(bp, bpExisted, oldHandler, &savedRegs)
		return nil, fmt.Errorf("wait for call return: %w", err)
	}
	if reason.Reason != ProcessStopped {
		t.restoreExprState(bp, bpExisted, oldHandler, &savedRegs)
		return nil, fmt.Errorf("process exited during function call")
	}

	// Read registers after the call (before restoring).
	callRegs := *proc.Registers()

	// Read the return value.
	var result *ExpressionResult
	if returnType != nil && returnSlot != nil {
		td, err := t.readReturnValue(funcDIE, returnType, *returnSlot, &callRegs)
		if err == nil {
			id := len(t.exprResults)
			t.exprResults = append(t.exprResults, td)
			result = &ExpressionResult{Value: td, ID: id}
		}
	}

	// Restore registers and breakpoint state.
	t.restoreExprState(bp, bpExisted, oldHandler, &savedRegs)

	return result, nil
}

// GetExpressionResult returns the TypedData for a previous expression
// result by its $N index. The data is re-read from memory in case the
// inferior has modified it since the expression was evaluated.
func (t *Target) GetExpressionResult(index int) (*TypedData, error) {
	if index < 0 || index >= len(t.exprResults) {
		return nil, fmt.Errorf("expression result $%d not found", index)
	}
	res := t.exprResults[index]
	// Re-read from memory if possible.
	if res.Address != nil {
		size := res.Type.Size()
		if size <= 0 {
			size = 8
		}
		data, err := t.process.ReadMemory(*res.Address, int(size))
		if err == nil {
			res = &TypedData{Data: data, Type: res.Type, Address: res.Address}
			t.exprResults[index] = res
		}
	}
	return res, nil
}

// restoreExprState restores the register and breakpoint state after an
// expression evaluation (whether it succeeded or failed).
func (t *Target) restoreExprState(bp *BreakpointSite, bpExisted bool, oldHandler func() bool, savedRegs *Registers) {
	proc := t.process
	regs := proc.Registers()
	if regs != nil {
		*regs = *savedRegs
		regs.WriteGPRs()
		regs.WriteFPRs()
	}
	if bpExisted {
		bp.InstallHitHandler(oldHandler)
	} else {
		bp.Disable()
		proc.breakpointSites.removeByAddress(bp.address)
	}
}

// getEntryPointAddress returns the actual entry point address from the
// auxiliary vector. This is the virtual address where the program started,
// which we reuse as the return address for inferior calls.
func (t *Target) getEntryPointAddress() (uint64, error) {
	auxv, err := readAuxv(t.process.pid)
	if err != nil {
		return 0, err
	}
	addr, ok := auxv[atEntry]
	if !ok {
		return 0, fmt.Errorf("AT_ENTRY not found in auxiliary vector")
	}
	return addr, nil
}

// ---------------------------------------------------------------------------
// Function Lookup
// ---------------------------------------------------------------------------

// findCallableFunction locates a function by name across all loaded ELFs.
// It tries DWARF info first (for parameter type information), then falls
// back to the ELF symbol table. Returns the virtual address to call and
// the DWARF entry (nil if found via symbol table only).
func (t *Target) findCallableFunction(name string) (uint64, *dwarf.Entry, error) {
	// First, try DWARF info across all ELFs — this gives us parameter types.
	var funcAddr uint64
	var funcDIE *dwarf.Entry
	found := false

	t.elves.ForEach(func(e *ELF) {
		if found {
			return
		}
		dw := e.DWARF()
		if dw == nil {
			return
		}
		fns := dw.FunctionsByName(name)
		for _, fn := range fns {
			if fn.IsInlined {
				continue
			}
			// Read the DIE to get parameter info.
			entry, err := dw.ReadDIE(fn.DIEOffset)
			if err != nil {
				continue
			}
			// Use the function's low PC + load bias as the call address.
			if len(fn.Ranges) > 0 {
				funcAddr = fn.Ranges[0][0] + e.LoadBias()
				funcDIE = entry
				found = true
				return
			}
		}
	})

	if found {
		return funcAddr, funcDIE, nil
	}

	// Fall back to ELF symbol table (no parameter type info).
	t.elves.ForEach(func(e *ELF) {
		if found {
			return
		}
		for _, sym := range e.SymbolsByName(name) {
			if elf.ST_TYPE(sym.Info) == elf.STT_FUNC && sym.Value != 0 {
				funcAddr = sym.Value + e.LoadBias()
				found = true
				return
			}
		}
	})

	if found {
		return funcAddr, nil, nil
	}

	return 0, nil, fmt.Errorf("function %q not found", name)
}

// getFunctionReturnType returns the DWARF type for a function's return
// value, or nil if the function returns void.
func (t *Target) getFunctionReturnType(funcDIE *dwarf.Entry) dwarf.Type {
	typeOff, ok := funcDIE.Val(dwarf.AttrType).(dwarf.Offset)
	if !ok {
		return nil // void function
	}
	// We need a *DWARF to resolve the type. Find it from the function's address.
	dw := t.dwarf()
	if dw == nil {
		return nil
	}
	typ, err := dw.data.Type(typeOff)
	if err != nil {
		return nil
	}
	return typ
}

// getFunctionParameters reads the DW_TAG_formal_parameter children of
// a function DIE and returns their types in order.
func (t *Target) getFunctionParameters(funcDIE *dwarf.Entry) []dwarf.Type {
	dw := t.dwarf()
	if dw == nil || funcDIE == nil {
		return nil
	}

	reader := dw.data.Reader()
	reader.Seek(funcDIE.Offset)
	// Read past the function DIE itself.
	reader.Next()

	if !funcDIE.Children {
		return nil
	}

	var params []dwarf.Type
	for {
		child, err := reader.Next()
		if err != nil || child == nil || child.Tag == 0 {
			break
		}
		if child.Tag == dwarf.TagFormalParameter {
			typeOff, ok := child.Val(dwarf.AttrType).(dwarf.Offset)
			if ok {
				typ, err := dw.data.Type(typeOff)
				if err == nil {
					params = append(params, typ)
				}
			}
		}
		reader.SkipChildren()
	}
	return params
}

// ---------------------------------------------------------------------------
// Argument Parsing
// ---------------------------------------------------------------------------

// parseArguments splits the argument string and parses each argument
// into an exprArg. Supports: integers, floats, strings, chars, bools,
// variable names, and $N expression result references.
func (t *Target) parseArguments(argString string) ([]exprArg, error) {
	if argString == "" {
		return nil, nil
	}

	parts := splitArguments(argString)
	var args []exprArg

	for _, part := range parts {
		arg, err := t.parseSingleArgument(part)
		if err != nil {
			return nil, fmt.Errorf("argument %q: %w", part, err)
		}
		args = append(args, arg)
	}

	return args, nil
}

// splitArguments splits a comma-separated argument list, respecting
// string literals (which may contain commas).
func splitArguments(s string) []string {
	var parts []string
	var current strings.Builder
	inString := false
	depth := 0

	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' && (i == 0 || s[i-1] != '\\') {
			inString = !inString
		}
		if !inString {
			if c == '(' {
				depth++
			} else if c == ')' {
				depth--
			} else if c == ',' && depth == 0 {
				parts = append(parts, strings.TrimSpace(current.String()))
				current.Reset()
				continue
			}
		}
		current.WriteByte(c)
	}

	if current.Len() > 0 {
		parts = append(parts, strings.TrimSpace(current.String()))
	}
	return parts
}

// parseSingleArgument parses one argument token into an exprArg.
func (t *Target) parseSingleArgument(arg string) (exprArg, error) {
	if arg == "" {
		return exprArg{}, fmt.Errorf("empty argument")
	}

	// String literal: "..."
	if len(arg) >= 2 && arg[0] == '"' && arg[len(arg)-1] == '"' {
		return t.parseStringArg(arg[1 : len(arg)-1])
	}

	// Boolean literals.
	if arg == "true" {
		return exprArg{data: []byte{1}, builtin: builtinBool}, nil
	}
	if arg == "false" {
		return exprArg{data: []byte{0}, builtin: builtinBool}, nil
	}

	// Character literal: 'x'
	if len(arg) == 3 && arg[0] == '\'' && arg[2] == '\'' {
		return exprArg{data: []byte{arg[1]}, builtin: builtinChar}, nil
	}

	// Expression result reference: $N
	if arg[0] == '$' {
		index, err := strconv.Atoi(arg[1:])
		if err != nil {
			return exprArg{}, fmt.Errorf("invalid expression result index %q", arg)
		}
		td, err := t.GetExpressionResult(index)
		if err != nil {
			return exprArg{}, err
		}
		return exprArg{data: td.Data, typ: td.Type, address: td.Address}, nil
	}

	// Numeric: starts with digit or '-'
	if arg[0] == '-' || (arg[0] >= '0' && arg[0] <= '9') {
		// Float if contains '.'
		if strings.Contains(arg, ".") {
			f, err := strconv.ParseFloat(arg, 64)
			if err != nil {
				return exprArg{}, fmt.Errorf("invalid float %q", arg)
			}
			buf := make([]byte, 8)
			binary.LittleEndian.PutUint64(buf, math.Float64bits(f))
			return exprArg{data: buf, builtin: builtinFloat}, nil
		}
		// Integer
		i, err := strconv.ParseInt(arg, 0, 64)
		if err != nil {
			return exprArg{}, fmt.Errorf("invalid integer %q", arg)
		}
		buf := make([]byte, 8)
		binary.LittleEndian.PutUint64(buf, uint64(i))
		return exprArg{data: buf, builtin: builtinInt}, nil
	}

	// Variable name (possibly indirect: var.field, var->field, var[i])
	td, err := t.ResolveIndirectName(arg)
	if err != nil {
		return exprArg{}, fmt.Errorf("resolve variable %q: %w", arg, err)
	}
	return exprArg{data: td.Data, typ: td.Type, address: td.Address}, nil
}

// parseStringArg allocates memory in the inferior for a string and
// returns an exprArg pointing to it.
func (t *Target) parseStringArg(s string) (exprArg, error) {
	// Allocate space for the string + null terminator.
	size := len(s) + 1
	ptr, err := t.inferiorMalloc(size)
	if err != nil {
		return exprArg{}, fmt.Errorf("allocate string: %w", err)
	}

	// Write the string data into the inferior's memory.
	data := append([]byte(s), 0) // null-terminated
	if err := t.process.WriteMemory(ptr, data); err != nil {
		return exprArg{}, fmt.Errorf("write string data: %w", err)
	}

	// The argument is a pointer to the string.
	buf := make([]byte, 8)
	binary.LittleEndian.PutUint64(buf, ptr)
	return exprArg{data: buf, builtin: builtinString}, nil
}

// ---------------------------------------------------------------------------
// SysV x86-64 ABI: Parameter Classification
// ---------------------------------------------------------------------------

// classifyType returns the parameter class for a type's eightbytes.
// We return [2]parameterClass for up to 2 eightbytes (16 bytes max in
// registers). For simplicity, we don't support vector types or complex.
func classifyType(typ dwarf.Type, builtin builtinArgType) [2]parameterClass {
	classes := [2]parameterClass{paramClassNoClass, paramClassNoClass}

	// Handle builtin literal types.
	if builtin != builtinNone {
		switch builtin {
		case builtinInt, builtinBool, builtinChar, builtinString:
			classes[0] = paramClassInteger
		case builtinFloat:
			classes[0] = paramClassSSE
		}
		return classes
	}

	if typ == nil {
		classes[0] = paramClassInteger
		return classes
	}

	stripped := stripQualifiers(typ)

	// Pointer/reference types → INTEGER.
	if _, ok := stripped.(*dwarf.PtrType); ok {
		classes[0] = paramClassInteger
		return classes
	}

	// Function type → INTEGER (pointer to function).
	if _, ok := stripped.(*dwarf.FuncType); ok {
		classes[0] = paramClassInteger
		return classes
	}

	// Enum → INTEGER.
	if _, ok := stripped.(*dwarf.EnumType); ok {
		classes[0] = paramClassInteger
		return classes
	}

	// Basic types.
	if bt := toBasicType(stripped); bt != nil {
		name := strings.ToLower(bt.Name)
		if name == "float" || name == "double" {
			classes[0] = paramClassSSE
		} else if name == "long double" {
			classes[0] = paramClassX87
			classes[1] = paramClassX87Up
		} else {
			classes[0] = paramClassInteger
		}
		return classes
	}

	// Struct/union/array types.
	if st, ok := stripped.(*dwarf.StructType); ok {
		return classifyStructType(st)
	}
	if at, ok := stripped.(*dwarf.ArrayType); ok {
		return classifyArrayType(at)
	}

	// Default: INTEGER.
	classes[0] = paramClassInteger
	return classes
}

// classifyStructType classifies a struct/union for ABI parameter passing.
func classifyStructType(st *dwarf.StructType) [2]parameterClass {
	classes := [2]parameterClass{paramClassNoClass, paramClassNoClass}

	size := st.Size()
	if size > 16 {
		// Too large for registers — pass in memory.
		return [2]parameterClass{paramClassMemory, paramClassMemory}
	}

	for _, field := range st.Field {
		if field.Name == "" {
			continue
		}
		byteOff := fieldByteOffset(field)
		eightbyteIdx := int(byteOff / 8)
		if eightbyteIdx > 1 {
			eightbyteIdx = 1
		}

		fieldClasses := classifyType(field.Type, builtinNone)
		classes[eightbyteIdx] = mergeParamClass(classes[eightbyteIdx], fieldClasses[0])
		if eightbyteIdx == 0 && fieldClasses[1] != paramClassNoClass {
			classes[1] = mergeParamClass(classes[1], fieldClasses[1])
		}
	}

	// Post-merger cleanup.
	if classes[0] == paramClassMemory || classes[1] == paramClassMemory {
		return [2]parameterClass{paramClassMemory, paramClassMemory}
	}

	return classes
}

// classifyArrayType classifies an array for ABI parameter passing.
func classifyArrayType(at *dwarf.ArrayType) [2]parameterClass {
	size := at.Size()
	if size > 16 {
		return [2]parameterClass{paramClassMemory, paramClassMemory}
	}

	elemClasses := classifyType(at.Type, builtinNone)
	classes := elemClasses
	if size > 8 && classes[1] == paramClassNoClass {
		classes[1] = classes[0]
	}
	return classes
}

// mergeParamClass merges two parameter classes for the same eightbyte.
func mergeParamClass(a, b parameterClass) parameterClass {
	if a == b {
		return a
	}
	if a == paramClassNoClass {
		return b
	}
	if b == paramClassNoClass {
		return a
	}
	if a == paramClassMemory || b == paramClassMemory {
		return paramClassMemory
	}
	if a == paramClassInteger || b == paramClassInteger {
		return paramClassInteger
	}
	if a == paramClassX87 || b == paramClassX87 ||
		a == paramClassX87Up || b == paramClassX87Up {
		return paramClassMemory
	}
	return paramClassSSE
}

// ---------------------------------------------------------------------------
// Argument Setup (SysV ABI)
// ---------------------------------------------------------------------------

// setupArguments arranges function arguments in registers and on the
// stack according to the SysV x86-64 calling convention.
func (t *Target) setupArguments(args []exprArg, funcDIE *dwarf.Entry, regs *Registers, returnSlot *uint64, returnType dwarf.Type) error {
	// ABI register sequences for argument passing.
	intRegNames := []string{"rdi", "rsi", "rdx", "rcx", "r8", "r9"}
	sseRegNames := []string{"xmm0", "xmm1", "xmm2", "xmm3", "xmm4", "xmm5", "xmm6", "xmm7"}

	currentIntReg := 0
	currentSSEReg := 0

	// If the return type is MEMORY class, the first integer register
	// holds a pointer to the return storage.
	if returnType != nil && returnSlot != nil {
		retClasses := classifyType(returnType, builtinNone)
		if retClasses[0] == paramClassMemory {
			if currentIntReg >= len(intRegNames) {
				return fmt.Errorf("no register available for return slot")
			}
			info, _ := RegisterInfoByName(intRegNames[currentIntReg])
			if err := regs.Write(info, *returnSlot); err != nil {
				return err
			}
			currentIntReg++
		}
	}

	// Get parameter types from DWARF if available.
	paramTypes := t.getFunctionParameters(funcDIE)

	type stackArg struct {
		data []byte
		size int // rounded up to eightbyte
	}
	var stackArgs []stackArg

	for i, arg := range args {
		// Determine the type for classification.
		var argType dwarf.Type
		if i < len(paramTypes) {
			argType = paramTypes[i]
		} else {
			argType = arg.typ
		}

		classes := classifyType(argType, arg.builtin)
		argData := arg.data

		// Ensure arg data is at least 8 bytes for register writes.
		if len(argData) < 8 {
			padded := make([]byte, 8)
			copy(padded, argData)
			argData = padded
		}

		// Count required registers.
		var paramSize int64
		if argType != nil {
			paramSize = argType.Size()
		}
		if paramSize <= 0 {
			paramSize = int64(len(arg.data))
		}
		if paramSize <= 0 {
			paramSize = 8
		}

		requiredInt := 0
		requiredSSE := 0
		for _, c := range classes {
			switch c {
			case paramClassInteger:
				requiredInt++
			case paramClassSSE:
				requiredSSE++
			}
		}

		// If not enough registers or class is MEMORY/X87, pass on stack.
		if currentIntReg+requiredInt > len(intRegNames) ||
			currentSSEReg+requiredSSE > len(sseRegNames) ||
			(requiredInt == 0 && requiredSSE == 0) ||
			classes[0] == paramClassMemory ||
			classes[0] == paramClassX87 {

			size := roundUpToEightbyte(int(paramSize))
			padded := make([]byte, size)
			copy(padded, argData)
			stackArgs = append(stackArgs, stackArg{data: padded, size: size})
			continue
		}

		// Allocate to registers.
		for j := 0; int64(j)*8 < paramSize; j++ {
			if j >= 2 {
				break
			}
			switch classes[j] {
			case paramClassInteger:
				if currentIntReg < len(intRegNames) {
					info, _ := RegisterInfoByName(intRegNames[currentIntReg])
					val := uint64(0)
					start := j * 8
					end := start + 8
					if end > len(argData) {
						end = len(argData)
					}
					if end > start {
						val = binary.LittleEndian.Uint64(padToEight(argData[start:end]))
					}
					if err := regs.Write(info, val); err != nil {
						return err
					}
					currentIntReg++
				}
			case paramClassSSE:
				if currentSSEReg < len(sseRegNames) {
					info, _ := RegisterInfoByName(sseRegNames[currentSSEReg])
					var val [16]byte
					start := j * 8
					end := start + 8
					if end > len(argData) {
						end = len(argData)
					}
					if end > start {
						copy(val[:], argData[start:end])
					}
					if err := regs.Write(info, val); err != nil {
						return err
					}
					currentSSEReg++
				}
			}
		}
	}

	// Push stack arguments in reverse order (right to left).
	if len(stackArgs) > 0 {
		rspInfo, _ := RegisterInfoByName("rsp")
		rsp := regs.Read(rspInfo).(uint64)

		// Reserve space for all stack args.
		totalSize := 0
		for _, sa := range stackArgs {
			totalSize += sa.size
		}
		rsp -= uint64(totalSize)
		// Align to 16-byte boundary.
		rsp &= ^uint64(0xf)

		// Write stack args left to right (leftmost at lowest address).
		pos := rsp
		for _, sa := range stackArgs {
			if err := t.process.WriteMemory(pos, sa.data); err != nil {
				return fmt.Errorf("write stack arg: %w", err)
			}
			pos += uint64(sa.size)
		}

		if err := regs.Write(rspInfo, rsp); err != nil {
			return err
		}
	}

	// Write the number of SSE registers used to rax (for varargs).
	raxInfo, _ := RegisterInfoByName("rax")
	if err := regs.Write(raxInfo, uint64(currentSSEReg)); err != nil {
		return err
	}

	return nil
}

// padToEight pads a byte slice to 8 bytes.
func padToEight(b []byte) []byte {
	if len(b) >= 8 {
		return b[:8]
	}
	result := make([]byte, 8)
	copy(result, b)
	return result
}

// roundUpToEightbyte rounds a size up to the nearest multiple of 8.
func roundUpToEightbyte(size int) int {
	return (size + 7) &^ 7
}

// ---------------------------------------------------------------------------
// Return Value Reading
// ---------------------------------------------------------------------------

// readReturnValue extracts the function's return value from the post-call
// register state, following the SysV x86-64 ABI rules.
func (t *Target) readReturnValue(funcDIE *dwarf.Entry, returnType dwarf.Type, returnSlot uint64, regs *Registers) (*TypedData, error) {
	classes := classifyType(returnType, builtinNone)

	// MEMORY class: return value was written to the return slot by the callee.
	if classes[0] == paramClassMemory {
		size := returnType.Size()
		if size <= 0 {
			size = 8
		}
		data, err := t.process.ReadMemory(returnSlot, int(size))
		if err != nil {
			return nil, err
		}
		addr := returnSlot
		return &TypedData{Data: data, Type: returnType, Address: &addr}, nil
	}

	// X87 class: long double in st0.
	if classes[0] == paramClassX87 {
		st0Info, _ := RegisterInfoByName("st0")
		val := regs.Read(st0Info)
		data := registerValueToBytes(val, st0Info.Size)
		if err := t.process.WriteMemory(returnSlot, data); err != nil {
			return nil, err
		}
		addr := returnSlot
		return &TypedData{Data: data, Type: returnType, Address: &addr}, nil
	}

	// INTEGER and SSE classes: read from rax/rdx and xmm0/xmm1.
	usedInt := false
	usedSSE := false
	var value []byte

	for _, class := range classes {
		switch class {
		case paramClassInteger:
			var regName string
			if !usedInt {
				regName = "rax"
				usedInt = true
			} else {
				regName = "rdx"
			}
			info, _ := RegisterInfoByName(regName)
			val := regs.Read(info).(uint64)
			buf := make([]byte, 8)
			binary.LittleEndian.PutUint64(buf, val)
			value = append(value, buf...)

		case paramClassSSE:
			var regName string
			if !usedSSE {
				regName = "xmm0"
				usedSSE = true
			} else {
				regName = "xmm1"
			}
			info, _ := RegisterInfoByName(regName)
			val := regs.Read(info)
			data := registerValueToBytes(val, info.Size)
			value = append(value, data[:8]...)

		case paramClassNoClass:
			// Skip.
		}
	}

	if len(value) == 0 {
		return nil, fmt.Errorf("could not read return value")
	}

	// Trim to actual type size.
	size := returnType.Size()
	if size > 0 && int(size) < len(value) {
		value = value[:size]
	}

	// Write to return slot for later access.
	if err := t.process.WriteMemory(returnSlot, value); err != nil {
		return nil, err
	}
	addr := returnSlot
	return &TypedData{Data: value, Type: returnType, Address: &addr}, nil
}

// ---------------------------------------------------------------------------
// Inferior malloc
// ---------------------------------------------------------------------------

// inferiorMalloc calls malloc() inside the inferior process and returns
// the allocated address. This is used to allocate memory for string
// arguments and return value storage.
func (t *Target) inferiorMalloc(size int) (uint64, error) {
	// Find malloc in any loaded ELF.
	mallocAddr, err := t.findSymbolAddress("malloc")
	if err != nil {
		return 0, fmt.Errorf("malloc not found: %w", err)
	}

	entryAddr, err := t.getEntryPointAddress()
	if err != nil {
		return 0, err
	}

	proc := t.process
	regs := proc.Registers()
	if regs == nil {
		return 0, fmt.Errorf("registers not available")
	}
	savedRegs := *regs

	// Ensure we have a breakpoint at the entry point.
	bp, bpExisted := proc.breakpointSites.getByAddress(entryAddr)
	if !bpExisted {
		var bpErr error
		bp, bpErr = proc.CreateBreakpointSite(entryAddr, false, true)
		if bpErr != nil {
			return 0, fmt.Errorf("create malloc return BP: %w", bpErr)
		}
		bp.Enable()
	}
	oldHandler := bp.onHit
	bp.InstallHitHandler(func() bool {
		return false
	})

	// Set up: rdi = size, rip = malloc, push return addr.
	rdiInfo, _ := RegisterInfoByName("rdi")
	regs.Write(rdiInfo, uint64(size))

	ripInfo, _ := RegisterInfoByName("rip")
	regs.Write(ripInfo, mallocAddr)

	rspInfo, _ := RegisterInfoByName("rsp")
	rsp := regs.Read(rspInfo).(uint64)
	rsp -= 8
	retAddrBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(retAddrBytes, entryAddr)
	proc.WriteMemory(rsp, retAddrBytes)
	regs.Write(rspInfo, rsp)

	// Also set rax to 0 (for varargs safety).
	raxInfo, _ := RegisterInfoByName("rax")
	regs.Write(raxInfo, uint64(0))

	// Call malloc.
	if err := proc.Resume(); err != nil {
		t.restoreExprState(bp, bpExisted, oldHandler, &savedRegs)
		return 0, fmt.Errorf("resume for malloc: %w", err)
	}
	reason, err := proc.WaitOnSignal()
	if err != nil {
		t.restoreExprState(bp, bpExisted, oldHandler, &savedRegs)
		return 0, fmt.Errorf("wait for malloc: %w", err)
	}
	if reason.Reason != ProcessStopped {
		t.restoreExprState(bp, bpExisted, oldHandler, &savedRegs)
		return 0, fmt.Errorf("process exited during malloc call")
	}

	// Read result from rax.
	result := proc.Registers().Read(raxInfo).(uint64)

	// Restore.
	t.restoreExprState(bp, bpExisted, oldHandler, &savedRegs)

	if result == 0 {
		return 0, fmt.Errorf("malloc returned NULL")
	}

	return result, nil
}

// findSymbolAddress searches all loaded ELFs for a function symbol with
// the given name and returns its virtual address.
func (t *Target) findSymbolAddress(name string) (uint64, error) {
	var addr uint64
	found := false

	t.elves.ForEach(func(e *ELF) {
		if found {
			return
		}
		for _, sym := range e.SymbolsByName(name) {
			if sym.Value != 0 && elf.ST_TYPE(sym.Info) == elf.STT_FUNC {
				addr = sym.Value + e.LoadBias()
				found = true
				return
			}
		}
	})

	if !found {
		return 0, fmt.Errorf("symbol %q not found", name)
	}
	return addr, nil
}
