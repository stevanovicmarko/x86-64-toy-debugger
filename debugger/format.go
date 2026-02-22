package debugger

import "fmt"

// FormatRegisterValue formats a register value for display.
// The output depends on the register's Format:
//
//   - Uint: "0x%0Nx" where N is 2×Size (e.g., 0x00000000cafebabe for 8 bytes)
//   - DoubleFloat: "%g"
//   - LongDouble: "[0x01,0x02,...]" (raw bytes)
//   - Vector: "[0x01,0x02,...]"
func FormatRegisterValue(info RegisterInfo, val any) string {
	switch info.Format {
	case RegisterFormatUint:
		return formatUint(info.Size, val)
	case RegisterFormatDoubleFloat:
		if f, ok := val.(float64); ok {
			return fmt.Sprintf("%g", f)
		}
		return fmt.Sprintf("%v", val)
	case RegisterFormatLongDouble:
		if b, ok := val.([16]byte); ok {
			return formatBytes(b[:])
		}
		return fmt.Sprintf("%v", val)
	case RegisterFormatVector:
		switch b := val.(type) {
		case [8]byte:
			return formatBytes(b[:])
		case [16]byte:
			return formatBytes(b[:])
		}
		return fmt.Sprintf("%v", val)
	default:
		return fmt.Sprintf("%v", val)
	}
}

func formatUint(size int, val any) string {
	var v uint64
	switch n := val.(type) {
	case uint8:
		v = uint64(n)
	case uint16:
		v = uint64(n)
	case uint32:
		v = uint64(n)
	case uint64:
		v = n
	default:
		return fmt.Sprintf("%v", val)
	}
	width := size * 2
	return fmt.Sprintf("0x%0*x", width, v)
}

func formatBytes(b []byte) string {
	s := "["
	for i, v := range b {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf("0x%02x", v)
	}
	s += "]"
	return s
}
