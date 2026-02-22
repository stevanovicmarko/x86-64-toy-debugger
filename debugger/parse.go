package debugger

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseRegisterValue parses a string into the appropriate Go type
// for the given register, based on its Format and Size.
func ParseRegisterValue(info RegisterInfo, s string) (any, error) {
	switch info.Format {
	case RegisterFormatUint:
		return parseUintValue(info.Size, s)
	case RegisterFormatDoubleFloat:
		return parseFloatValue(s)
	case RegisterFormatLongDouble:
		// LongDouble registers are 16 bytes; parse as a vector.
		return parseVectorValue(16, s)
	case RegisterFormatVector:
		return parseVectorValue(info.Size, s)
	default:
		return nil, fmt.Errorf("unsupported register format for %s", info.Name)
	}
}

// parseUintValue parses a string as an unsigned integer and returns
// the appropriately-sized Go type. Base 0 means "0x" prefix selects hex.
func parseUintValue(size int, s string) (any, error) {
	v, err := strconv.ParseUint(s, 0, size*8)
	if err != nil {
		return nil, fmt.Errorf("invalid integer %q: %v", s, err)
	}
	switch size {
	case 1:
		return uint8(v), nil
	case 2:
		return uint16(v), nil
	case 4:
		return uint32(v), nil
	case 8:
		return uint64(v), nil
	default:
		return nil, fmt.Errorf("unsupported uint size %d", size)
	}
}

// parseFloatValue parses a string as a 64-bit floating-point number.
func parseFloatValue(s string) (any, error) {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid float %q: %v", s, err)
	}
	return v, nil
}

// parseVectorValue parses a string like "[0x01,0x02,...]" into an
// [8]byte or [16]byte value.
func parseVectorValue(size int, s string) (any, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, fmt.Errorf("vector must be enclosed in brackets: %q", s)
	}
	inner := s[1 : len(s)-1]
	parts := strings.Split(inner, ",")
	if len(parts) != size {
		return nil, fmt.Errorf("expected %d bytes, got %d", size, len(parts))
	}

	var buf [16]byte
	for i, p := range parts {
		v, err := strconv.ParseUint(strings.TrimSpace(p), 0, 8)
		if err != nil {
			return nil, fmt.Errorf("invalid byte at position %d: %v", i, err)
		}
		buf[i] = byte(v)
	}

	if size == 8 {
		var out [8]byte
		copy(out[:], buf[:8])
		return out, nil
	}
	var out [16]byte
	copy(out[:], buf[:16])
	return out, nil
}
