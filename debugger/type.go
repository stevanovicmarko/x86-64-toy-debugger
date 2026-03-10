package debugger

import (
	"debug/dwarf"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// ---------------------------------------------------------------------------
// TypedData — Typed Variable Data
//
// TypedData combines raw bytes with their DWARF type and optional memory
// address. It supports visualization of all major C/C++ type categories
// and navigation through struct members, pointers, and array indices.
//
// The type information comes from Go's debug/dwarf package, which
// resolves DWARF type DIEs into a rich Go type hierarchy (BasicType,
// PtrType, StructType, ArrayType, QualType, TypedefType, etc.).
// This avoids manually walking type DIE chains.
// ---------------------------------------------------------------------------

// TypedData holds the raw bytes of a variable along with its DWARF type
// and optional virtual address (absent for register-resident values).
type TypedData struct {
	Data    []byte
	Type    dwarf.Type
	Address *uint64 // nil if the data has no memory address (e.g., in a register)
}

// Visualize returns a human-readable string representation of the typed
// data. It dispatches to type-specific formatters based on the underlying
// DWARF type. depth controls indentation for nested struct members.
func (td *TypedData) Visualize(proc *Process, depth int) string {
	stripped := stripQualifiers(td.Type)

	// Try to extract a BasicType from Go's specific dwarf subtypes.
	// Go's debug/dwarf creates distinct types (IntType, UintType, CharType, etc.)
	// that embed BasicType but don't match *dwarf.BasicType in a type switch.
	if bt := toBasicType(stripped); bt != nil {
		return visualizeBasicType(td.Data, bt)
	}

	switch t := stripped.(type) {
	case *dwarf.PtrType:
		return visualizePointerType(proc, td.Data, t)
	case *dwarf.ArrayType:
		return visualizeArrayType(proc, td.Data, t)
	case *dwarf.StructType:
		return visualizeStructType(proc, td.Data, td.Address, t, depth)
	case *dwarf.EnumType:
		return visualizeEnumType(td.Data, t)
	case *dwarf.FuncType:
		if len(td.Data) >= 8 {
			ptr := binary.LittleEndian.Uint64(td.Data)
			return fmt.Sprintf("0x%x", ptr)
		}
		return "<function>"
	case *dwarf.UnspecifiedType:
		return "<unspecified>"
	default:
		return fmt.Sprintf("<unsupported type: %T>", stripped)
	}
}

// toBasicType extracts the embedded *dwarf.BasicType from any of Go's
// debug/dwarf specific integer/float/char/bool types. Returns nil if
// the type is not a basic type variant.
func toBasicType(t dwarf.Type) *dwarf.BasicType {
	switch v := t.(type) {
	case *dwarf.BasicType:
		return v
	case *dwarf.IntType:
		return &v.BasicType
	case *dwarf.UintType:
		return &v.BasicType
	case *dwarf.FloatType:
		return &v.BasicType
	case *dwarf.CharType:
		return &v.BasicType
	case *dwarf.UcharType:
		return &v.BasicType
	case *dwarf.BoolType:
		return &v.BasicType
	case *dwarf.ComplexType:
		return &v.BasicType
	default:
		return nil
	}
}

// DerefPointer interprets the data as a pointer and reads the pointed-to
// value from process memory.
func (td *TypedData) DerefPointer(proc *Process) (*TypedData, error) {
	pt, ok := stripQualifiers(td.Type).(*dwarf.PtrType)
	if !ok {
		return nil, fmt.Errorf("not a pointer type")
	}
	if len(td.Data) < 8 {
		return nil, fmt.Errorf("pointer data too short")
	}
	addr := binary.LittleEndian.Uint64(td.Data)
	valueType := pt.Type
	size := valueType.Size()
	if size <= 0 {
		size = 8 // default for opaque types
	}
	data, err := proc.ReadMemory(addr, int(size))
	if err != nil {
		return nil, fmt.Errorf("deref pointer at 0x%x: %w", addr, err)
	}
	return &TypedData{Data: data, Type: valueType, Address: &addr}, nil
}

// ReadMember reads a named struct/union member from the typed data.
func (td *TypedData) ReadMember(proc *Process, memberName string) (*TypedData, error) {
	st, ok := stripQualifiers(td.Type).(*dwarf.StructType)
	if !ok {
		return nil, fmt.Errorf("not a struct type")
	}
	for _, field := range st.Field {
		if field.Name != memberName {
			continue
		}
		byteOffset := fieldByteOffset(field)
		memberType := field.Type
		memberSize := memberType.Size()
		if memberSize <= 0 {
			memberSize = 8
		}

		end := byteOffset + memberSize
		if end > int64(len(td.Data)) {
			end = int64(len(td.Data))
		}
		memberData := make([]byte, memberSize)
		copy(memberData, td.Data[byteOffset:end])

		// Handle bitfields.
		if field.BitSize > 0 {
			memberData = fixupBitfieldData(memberData, field)
		}

		var addr *uint64
		if td.Address != nil {
			a := *td.Address + uint64(byteOffset)
			addr = &a
		}
		return &TypedData{Data: memberData, Type: memberType, Address: addr}, nil
	}
	return nil, fmt.Errorf("no member %q in type %s", memberName, st.StructName)
}

// Index reads an element from an array or pointer type by index.
func (td *TypedData) Index(proc *Process, index int) (*TypedData, error) {
	stripped := stripQualifiers(td.Type)

	switch t := stripped.(type) {
	case *dwarf.PtrType:
		// Pointer indexing: read from memory at pointer + offset.
		if len(td.Data) < 8 {
			return nil, fmt.Errorf("pointer data too short")
		}
		baseAddr := binary.LittleEndian.Uint64(td.Data)
		elemType := t.Type
		elemSize := elemType.Size()
		if elemSize <= 0 {
			elemSize = 8
		}
		addr := baseAddr + uint64(index)*uint64(elemSize)
		data, err := proc.ReadMemory(addr, int(elemSize))
		if err != nil {
			return nil, fmt.Errorf("index pointer at 0x%x: %w", addr, err)
		}
		return &TypedData{Data: data, Type: elemType, Address: &addr}, nil

	case *dwarf.ArrayType:
		elemType := t.Type
		elemSize := elemType.Size()
		if elemSize <= 0 {
			elemSize = 8
		}
		offset := int64(index) * elemSize
		if offset+elemSize > int64(len(td.Data)) {
			return nil, fmt.Errorf("array index %d out of bounds", index)
		}
		elemData := make([]byte, elemSize)
		copy(elemData, td.Data[offset:offset+elemSize])
		var addr *uint64
		if td.Address != nil {
			a := *td.Address + uint64(offset)
			addr = &a
		}
		return &TypedData{Data: elemData, Type: elemType, Address: addr}, nil

	default:
		return nil, fmt.Errorf("not an array or pointer type")
	}
}

// ---------------------------------------------------------------------------
// Type Helpers
// ---------------------------------------------------------------------------

// stripQualifiers removes const, volatile, restrict, and typedef
// wrappers to get to the underlying concrete type.
func stripQualifiers(t dwarf.Type) dwarf.Type {
	for {
		switch v := t.(type) {
		case *dwarf.QualType:
			t = v.Type
		case *dwarf.TypedefType:
			t = v.Type
		default:
			return t
		}
	}
}

// isCharType checks whether a type is a character type. Go's debug/dwarf
// uses distinct types (*dwarf.CharType, *dwarf.UcharType) for character
// encodings, so we check both Go type identity and name as fallback.
func isCharType(t dwarf.Type) bool {
	stripped := stripQualifiers(t)
	switch stripped.(type) {
	case *dwarf.CharType, *dwarf.UcharType:
		return true
	}
	if bt := toBasicType(stripped); bt != nil {
		name := strings.ToLower(bt.Name)
		return name == "char" || name == "signed char" || name == "unsigned char"
	}
	return false
}

// ---------------------------------------------------------------------------
// Bitfield Support
// ---------------------------------------------------------------------------

// fieldByteOffset computes the byte offset for a struct field. When the
// field uses DW_AT_data_bit_offset (DWARF 4+) without a separate
// DW_AT_data_member_location, ByteOffset will be 0 and we must derive
// the byte position from the bit offset.
func fieldByteOffset(field *dwarf.StructField) int64 {
	if field.ByteOffset != 0 {
		return field.ByteOffset
	}
	if field.DataBitOffset != 0 {
		return field.DataBitOffset / 8
	}
	return 0
}

// fixupBitfieldData extracts bitfield data from the storage bytes,
// shifting and masking to produce a value as if it were a regular integer.
func fixupBitfieldData(data []byte, field *dwarf.StructField) []byte {
	if field.BitSize == 0 {
		return data
	}

	bitOffset := int64(0)
	if field.DataBitOffset != 0 {
		// DWARF 4+ preferred encoding: DW_AT_data_bit_offset gives
		// the offset from the start of the containing struct.
		// We already positioned data at the byte boundary, so take mod 8.
		bitOffset = field.DataBitOffset % 8
	} else if field.BitOffset != 0 {
		// Deprecated DWARF encoding: DW_AT_bit_offset is distance from
		// the MSB of the storage to the MSB of the data. Convert to
		// little-endian bit offset from the start.
		storageBitSize := int64(len(data)) * 8
		bitOffset = storageBitSize - field.BitOffset - field.BitSize
	}

	// Extract the bits into a fresh buffer.
	result := make([]byte, len(data))
	memcpyBits(result, 0, data, uint64(bitOffset), uint64(field.BitSize))
	return result
}

// ---------------------------------------------------------------------------
// Type-Specific Visualizers
// ---------------------------------------------------------------------------

func visualizeBasicType(data []byte, bt *dwarf.BasicType) string {
	size := bt.ByteSize
	if size <= 0 {
		size = int64(len(data))
	}
	if int(size) > len(data) {
		return "<incomplete data>"
	}

	name := strings.ToLower(bt.Name)

	// Check for boolean types.
	if name == "bool" || name == "_bool" {
		if len(data) > 0 && data[0] != 0 {
			return "true"
		}
		return "false"
	}

	// Floating-point types by name.
	if name == "float" && size == 4 {
		v := math.Float32frombits(binary.LittleEndian.Uint32(data[:4]))
		return fmt.Sprintf("%g", v)
	}
	if name == "double" && size == 8 {
		v := math.Float64frombits(binary.LittleEndian.Uint64(data[:8]))
		return fmt.Sprintf("%g", v)
	}
	if name == "long double" && size >= 10 {
		// long double on x86-64 is 80-bit extended, stored in 16 bytes.
		// Approximate by reading the first 8 bytes as double.
		v := math.Float64frombits(binary.LittleEndian.Uint64(data[:8]))
		return fmt.Sprintf("%g", v)
	}

	// Character types.
	if name == "char" || name == "signed char" {
		return fmt.Sprintf("'%c'", data[0])
	}
	if name == "unsigned char" {
		return fmt.Sprintf("'%c'", data[0])
	}

	// Signed integer types.
	if isSigned(name) {
		switch size {
		case 1:
			return fmt.Sprintf("%d", int8(data[0]))
		case 2:
			return fmt.Sprintf("%d", int16(binary.LittleEndian.Uint16(data[:2])))
		case 4:
			return fmt.Sprintf("%d", int32(binary.LittleEndian.Uint32(data[:4])))
		case 8:
			return fmt.Sprintf("%d", int64(binary.LittleEndian.Uint64(data[:8])))
		}
	}

	// Unsigned integer types (and fallback).
	switch size {
	case 1:
		return fmt.Sprintf("%d", data[0])
	case 2:
		return fmt.Sprintf("%d", binary.LittleEndian.Uint16(data[:2]))
	case 4:
		return fmt.Sprintf("%d", binary.LittleEndian.Uint32(data[:4]))
	case 8:
		return fmt.Sprintf("%d", binary.LittleEndian.Uint64(data[:8]))
	}

	return fmt.Sprintf("<unknown basic type %q size=%d>", bt.Name, size)
}

// isSigned checks whether a basic type name indicates a signed type.
func isSigned(name string) bool {
	if strings.HasPrefix(name, "unsigned") {
		return false
	}
	return name == "int" || name == "short" || name == "long" ||
		name == "long long" || name == "long int" || name == "long long int" ||
		name == "short int" || name == "signed char" ||
		strings.HasPrefix(name, "int") || strings.Contains(name, "signed")
}

func visualizePointerType(proc *Process, data []byte, pt *dwarf.PtrType) string {
	if len(data) < 8 {
		return "<incomplete pointer>"
	}
	ptr := binary.LittleEndian.Uint64(data[:8])
	if ptr == 0 {
		return "0x0"
	}

	// If pointing to a char type, read and display as a string.
	if isCharType(pt.Type) {
		s, err := readString(proc, ptr)
		if err == nil {
			return fmt.Sprintf("%q", s)
		}
	}

	return fmt.Sprintf("0x%x", ptr)
}

func visualizeArrayType(proc *Process, data []byte, at *dwarf.ArrayType) string {
	elemType := at.Type
	elemSize := elemType.Size()
	if elemSize <= 0 {
		return "[]"
	}

	count := at.Count
	if count <= 0 {
		// Zero-length or flexible array.
		return "[]"
	}

	var parts []string
	for i := int64(0); i < count; i++ {
		offset := i * elemSize
		end := offset + elemSize
		if end > int64(len(data)) {
			break
		}
		elemData := data[offset:end]
		elem := TypedData{Data: elemData, Type: elemType}
		parts = append(parts, elem.Visualize(proc, 0))
	}

	return "[" + strings.Join(parts, ", ") + "]"
}

func visualizeStructType(proc *Process, data []byte, addr *uint64, st *dwarf.StructType, depth int) string {
	if len(st.Field) == 0 {
		return "{}"
	}

	indent := strings.Repeat("\t", depth+1)
	closingIndent := strings.Repeat("\t", depth)

	var lines []string
	for _, field := range st.Field {
		if field.Name == "" {
			continue // skip anonymous/padding fields
		}

		byteOffset := fieldByteOffset(field)
		memberType := field.Type
		memberSize := memberType.Size()
		if memberSize <= 0 {
			memberSize = 8
		}

		end := byteOffset + memberSize
		if end > int64(len(data)) {
			end = int64(len(data))
		}
		if byteOffset >= int64(len(data)) {
			continue
		}
		memberData := make([]byte, memberSize)
		available := int64(len(data)) - byteOffset
		if available > memberSize {
			available = memberSize
		}
		copy(memberData, data[byteOffset:byteOffset+available])

		// Handle bitfields.
		if field.BitSize > 0 {
			memberData = fixupBitfieldData(memberData, field)
		}

		member := TypedData{Data: memberData, Type: memberType}
		memberStr := member.Visualize(proc, depth+1)
		lines = append(lines, fmt.Sprintf("%s%s: %s", indent, field.Name, memberStr))
	}

	return "{\n" + strings.Join(lines, "\n") + "\n" + closingIndent + "}"
}

func visualizeEnumType(data []byte, et *dwarf.EnumType) string {
	if len(data) == 0 {
		return "<empty enum>"
	}
	var val int64
	switch et.ByteSize {
	case 1:
		val = int64(int8(data[0]))
	case 2:
		val = int64(int16(binary.LittleEndian.Uint16(data[:2])))
	case 4:
		val = int64(int32(binary.LittleEndian.Uint32(data[:4])))
	case 8:
		val = int64(binary.LittleEndian.Uint64(data[:8]))
	default:
		val = int64(data[0])
	}

	// Try to find a matching enumerator name.
	for _, v := range et.Val {
		if v.Val == val {
			return v.Name
		}
	}
	return fmt.Sprintf("%d", val)
}

// readString reads a null-terminated string from process memory.
func readString(proc *Process, addr uint64) (string, error) {
	var result []byte
	for {
		chunk, err := proc.ReadMemory(addr, 256)
		if err != nil {
			return "", err
		}
		for _, b := range chunk {
			if b == 0 {
				return string(result), nil
			}
			result = append(result, b)
		}
		addr += 256
		// Safety limit to avoid infinite reads.
		if len(result) > 4096 {
			return string(result), nil
		}
	}
}
