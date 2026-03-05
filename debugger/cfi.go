package debugger

import (
	"encoding/binary"
	"fmt"
	"sort"
)

// ---------------------------------------------------------------------------
// Pointer encoding constants (DWARF Exception Header)
//
// These come from the LSB (Linux Standards Base) and the DWARF spec.
// A pointer encoding byte is split into two nibbles:
//   - Low nibble (format): how the value is stored (uleb128, 2/4/8 bytes, signed, etc.)
//   - High nibble (application): what the value is relative to (pcrel, textrel, etc.)
// ---------------------------------------------------------------------------

// Format (low nibble): how the pointer value is encoded.
const (
	dwPtrAbsptr  uint8 = 0x00 // native-width absolute pointer
	dwPtrUleb128 uint8 = 0x01 // unsigned LEB128
	dwPtrUdata2  uint8 = 0x02 // unsigned 2-byte
	dwPtrUdata4  uint8 = 0x03 // unsigned 4-byte
	dwPtrUdata8  uint8 = 0x04 // unsigned 8-byte
	dwPtrSleb128 uint8 = 0x09 // signed LEB128
	dwPtrSdata2  uint8 = 0x0a // signed 2-byte
	dwPtrSdata4  uint8 = 0x0b // signed 4-byte
	dwPtrSdata8  uint8 = 0x0c // signed 8-byte
)

// Application (high nibble): what the encoded value is relative to.
const (
	dwPtrPcrel   uint8 = 0x10 // relative to current position in the section
	dwPtrTextrel uint8 = 0x20 // relative to .text section start
	dwPtrDatarel uint8 = 0x30 // relative to .eh_frame_hdr section start
	dwPtrFuncrel uint8 = 0x40 // relative to start of function
)

// Special encoding.
const dwPtrOmit uint8 = 0xff // value is absent

// ---------------------------------------------------------------------------
// cfiReader — a stateful cursor over a byte slice
//
// The baseAddr field tracks the virtual address corresponding to data[0].
// This is critical for DW_EH_PE_pcrel decoding: the "current PC" during
// pointer decoding is baseAddr + pos (the address of the byte being read).
// ---------------------------------------------------------------------------

type cfiReader struct {
	data     []byte
	pos      int
	baseAddr uint64 // virtual address of data[0]
}

func (r *cfiReader) remaining() int {
	return len(r.data) - r.pos
}

func (r *cfiReader) seek(pos int) {
	r.pos = pos
}

// slice returns a sub-reader covering [r.pos, r.pos+n) with correct baseAddr.
func (r *cfiReader) slice(n int) (*cfiReader, error) {
	if r.pos+n > len(r.data) {
		return nil, fmt.Errorf("cfiReader.slice: want %d bytes at offset %d, have %d", n, r.pos, len(r.data))
	}
	sub := &cfiReader{
		data:     r.data[r.pos : r.pos+n],
		baseAddr: r.baseAddr + uint64(r.pos),
	}
	r.pos += n
	return sub, nil
}

func (r *cfiReader) readU8() (uint8, error) {
	if r.pos >= len(r.data) {
		return 0, fmt.Errorf("cfiReader.readU8: EOF at offset %d", r.pos)
	}
	v := r.data[r.pos]
	r.pos++
	return v, nil
}

func (r *cfiReader) readU16() (uint16, error) {
	if r.pos+2 > len(r.data) {
		return 0, fmt.Errorf("cfiReader.readU16: EOF at offset %d", r.pos)
	}
	v := binary.LittleEndian.Uint16(r.data[r.pos:])
	r.pos += 2
	return v, nil
}

func (r *cfiReader) readU32() (uint32, error) {
	if r.pos+4 > len(r.data) {
		return 0, fmt.Errorf("cfiReader.readU32: EOF at offset %d", r.pos)
	}
	v := binary.LittleEndian.Uint32(r.data[r.pos:])
	r.pos += 4
	return v, nil
}

func (r *cfiReader) readU64() (uint64, error) {
	if r.pos+8 > len(r.data) {
		return 0, fmt.Errorf("cfiReader.readU64: EOF at offset %d", r.pos)
	}
	v := binary.LittleEndian.Uint64(r.data[r.pos:])
	r.pos += 8
	return v, nil
}

// readULEB128 decodes an unsigned LEB128 value.
func (r *cfiReader) readULEB128() (uint64, error) {
	val, n := binary.Uvarint(r.data[r.pos:])
	if n <= 0 {
		return 0, fmt.Errorf("cfiReader.readULEB128: decode error at offset %d", r.pos)
	}
	r.pos += n
	return val, nil
}

// readSLEB128 decodes a signed LEB128 value.
// Go's binary.Varint uses zigzag encoding (not SLEB128), so we implement
// the DWARF SLEB128 algorithm directly: each byte contributes 7 bits,
// and the sign bit of the last byte is extended.
func (r *cfiReader) readSLEB128() (int64, error) {
	var result int64
	var shift uint
	for {
		if r.pos >= len(r.data) {
			return 0, fmt.Errorf("cfiReader.readSLEB128: EOF at offset %d", r.pos)
		}
		b := r.data[r.pos]
		r.pos++
		result |= int64(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			// Sign-extend if the sign bit (bit 6 of last byte) is set.
			if shift < 64 && b&0x40 != 0 {
				result |= -(1 << shift)
			}
			break
		}
	}
	return result, nil
}

// readAddress decodes a pointer using the DWARF pointer encoding scheme.
// The encoding byte combines a format (low nibble) with a relativity
// mode (high nibble). For pcrel, the base is the virtual address of
// the encoded value itself (baseAddr + pos before reading).
func (r *cfiReader) readAddress(enc uint8) (uint64, error) {
	if enc == dwPtrOmit {
		return 0, nil
	}

	// Remember position before reading — pcrel is relative to this point.
	addrOfValue := r.baseAddr + uint64(r.pos)

	var val uint64
	switch enc & 0x0f {
	case dwPtrAbsptr:
		v, err := r.readU64()
		if err != nil {
			return 0, err
		}
		val = v
	case dwPtrUleb128:
		v, err := r.readULEB128()
		if err != nil {
			return 0, err
		}
		val = v
	case dwPtrUdata2:
		v, err := r.readU16()
		if err != nil {
			return 0, err
		}
		val = uint64(v)
	case dwPtrUdata4:
		v, err := r.readU32()
		if err != nil {
			return 0, err
		}
		val = uint64(v)
	case dwPtrUdata8:
		v, err := r.readU64()
		if err != nil {
			return 0, err
		}
		val = v
	case dwPtrSleb128:
		v, err := r.readSLEB128()
		if err != nil {
			return 0, err
		}
		val = uint64(v)
	case dwPtrSdata2:
		v, err := r.readU16()
		if err != nil {
			return 0, err
		}
		val = uint64(int64(int16(v)))
	case dwPtrSdata4:
		v, err := r.readU32()
		if err != nil {
			return 0, err
		}
		val = uint64(int64(int32(v)))
	case dwPtrSdata8:
		v, err := r.readU64()
		if err != nil {
			return 0, err
		}
		val = v
	default:
		return 0, fmt.Errorf("unsupported pointer format 0x%02x", enc&0x0f)
	}

	// Apply relativity.
	switch enc & 0x70 {
	case 0x00: // absolute
		// val is already the final address
	case dwPtrPcrel:
		val += addrOfValue
	case dwPtrTextrel:
		// textrel requires .text base — not available in reader alone.
		// Callers must handle this at a higher level.
		return 0, fmt.Errorf("DW_EH_PE_textrel not supported in cfiReader")
	case dwPtrDatarel:
		// datarel is relative to the start of the section. The caller
		// must set baseAddr to the section start for this to work.
		// In .eh_frame_hdr, datarel is relative to the hdr section start.
		val += r.baseAddr
	default:
		return 0, fmt.Errorf("unsupported pointer application 0x%02x", enc&0x70)
	}

	return val, nil
}

// ---------------------------------------------------------------------------
// CIE — Common Information Entry
//
// A CIE holds the shared properties for a group of FDEs: alignment
// factors, return address register, and the initial CFI instructions.
// The augmentation string (e.g., "zR", "zPLR") describes optional
// extension data present after the standard fields.
// ---------------------------------------------------------------------------

// CIE represents a parsed Common Information Entry from .eh_frame.
type CIE struct {
	Length                uint32
	CodeAlignmentFactor   uint64
	DataAlignmentFactor   int64
	ReturnAddressRegister uint64
	HasAugmentation       bool
	FDEPointerEncoding    uint8
	LSDAPointerEncoding   uint8
	Instructions          []byte
}

// ---------------------------------------------------------------------------
// FDE — Frame Description Entry
//
// An FDE describes how to unwind one contiguous range of code. It
// references a CIE for shared properties and contains its own CFI
// instructions that build on the CIE's initial instructions.
// ---------------------------------------------------------------------------

// FDE represents a parsed Frame Description Entry from .eh_frame.
type FDE struct {
	Length          uint32
	CIE             *CIE
	InitialLocation uint64 // file address (before load bias)
	AddressRange    uint64
	Instructions    []byte
}

// ContainsPC reports whether the given file-address falls within
// this FDE's [InitialLocation, InitialLocation+AddressRange).
func (f *FDE) ContainsPC(fileAddr uint64) bool {
	return fileAddr >= f.InitialLocation && fileAddr < f.InitialLocation+f.AddressRange
}

// ---------------------------------------------------------------------------
// CallFrameInformation — the main CFI container
//
// Constructed from the raw .eh_frame and .eh_frame_hdr section bytes
// plus their virtual addresses. The .eh_frame_hdr provides a sorted
// table for O(log n) FDE lookup by PC; without it, FDEForPC falls
// back to a linear scan of .eh_frame.
// ---------------------------------------------------------------------------

// CallFrameInformation holds parsed .eh_frame data and provides
// FDE lookup by program counter.
type CallFrameInformation struct {
	ehFrameData []byte // raw .eh_frame contents
	ehFrameAddr uint64 // virtual address of .eh_frame section
	textStart   uint64 // .text section start address (for textrel)
	loadBias    uint64

	cieCache map[uint64]*CIE // .eh_frame offset → parsed CIE

	// .eh_frame_hdr binary search table
	hdrData       []byte // raw .eh_frame_hdr contents
	hdrAddr       uint64 // virtual address of .eh_frame_hdr
	hdrTableOff   int    // byte offset within hdrData where the table starts
	hdrTableCount uint64 // number of (initial_location, fde_pointer) pairs
	hdrTableEnc   uint8  // encoding of table entries
	hdrHasTable   bool   // true if .eh_frame_hdr was successfully parsed
}

// newCallFrameInformation constructs a CallFrameInformation from raw
// section data. If ehFrameHdr is non-nil, it is parsed for fast binary
// search lookup.
func newCallFrameInformation(ehFrame []byte, ehFrameAddr uint64,
	ehFrameHdr []byte, ehFrameHdrAddr uint64, textStart uint64) *CallFrameInformation {

	cfi := &CallFrameInformation{
		ehFrameData: ehFrame,
		ehFrameAddr: ehFrameAddr,
		textStart:   textStart,
		cieCache:    make(map[uint64]*CIE),
	}

	if len(ehFrameHdr) > 0 {
		cfi.hdrData = ehFrameHdr
		cfi.hdrAddr = ehFrameHdrAddr
		cfi.parseEhFrameHdr(ehFrameHdr, ehFrameHdrAddr)
	}

	return cfi
}

// parseEhFrameHdr parses the .eh_frame_hdr section, which provides:
//   - A pointer to .eh_frame (redundant — we already have it)
//   - A count of FDEs
//   - A sorted table of (initial_location, fde_pointer) pairs
//
// The table enables binary search instead of linear scan.
func (cfi *CallFrameInformation) parseEhFrameHdr(hdr []byte, hdrAddr uint64) {
	r := &cfiReader{data: hdr, baseAddr: hdrAddr}

	// Version (must be 1).
	version, err := r.readU8()
	if err != nil || version != 1 {
		return
	}

	// Encoding bytes for: eh_frame_ptr, fde_count, table entries.
	ehFramePtrEnc, err := r.readU8()
	if err != nil {
		return
	}
	fdeCountEnc, err := r.readU8()
	if err != nil {
		return
	}
	tableEnc, err := r.readU8()
	if err != nil {
		return
	}

	// eh_frame_ptr — we already know where .eh_frame is, but we must
	// read past it to advance the position.
	if _, err := r.readAddress(ehFramePtrEnc); err != nil {
		return
	}

	// FDE count.
	if fdeCountEnc == dwPtrOmit {
		return
	}
	fdeCount, err := r.readAddress(fdeCountEnc)
	if err != nil || fdeCount == 0 {
		return
	}

	cfi.hdrTableOff = r.pos
	cfi.hdrTableCount = fdeCount
	cfi.hdrTableEnc = tableEnc
	cfi.hdrHasTable = true
}

// getCIE returns the CIE at the given .eh_frame offset, using the
// cache to avoid re-parsing.
func (cfi *CallFrameInformation) getCIE(offset uint64) (*CIE, error) {
	if cie, ok := cfi.cieCache[offset]; ok {
		return cie, nil
	}
	if int(offset) >= len(cfi.ehFrameData) {
		return nil, fmt.Errorf("CIE offset %d out of range (section size %d)", offset, len(cfi.ehFrameData))
	}
	r := &cfiReader{
		data:     cfi.ehFrameData[offset:],
		baseAddr: cfi.ehFrameAddr + offset,
	}
	cie, err := parseCIE(r)
	if err != nil {
		return nil, fmt.Errorf("parse CIE at offset %d: %w", offset, err)
	}
	cfi.cieCache[offset] = cie
	return cie, nil
}

// parseCIE reads a CIE from the current reader position.
//
// .eh_frame CIE layout:
//
//	length         : u32 (0xffffffff → 64-bit extended, not supported)
//	CIE_id         : u32 (always 0 in .eh_frame; non-zero means FDE)
//	version        : u8  (1 or 3)
//	augmentation   : null-terminated string
//	code_align     : ULEB128
//	data_align     : SLEB128
//	return_reg     : ULEB128 (or u8 in version 1)
//	[augmentation data if 'z' present]
//	initial_instructions : remaining bytes
func parseCIE(r *cfiReader) (*CIE, error) {
	cie := &CIE{}

	length, err := r.readU32()
	if err != nil {
		return nil, err
	}
	if length == 0 {
		return nil, fmt.Errorf("zero-length CIE")
	}
	if length == 0xffffffff {
		return nil, fmt.Errorf("64-bit DWARF CIE not supported")
	}
	cie.Length = length

	// Create a sub-reader for the CIE body (length bytes after the length field).
	body, err := r.slice(int(length))
	if err != nil {
		return nil, err
	}

	// CIE ID — must be 0 for .eh_frame CIEs.
	cieID, err := body.readU32()
	if err != nil {
		return nil, err
	}
	if cieID != 0 {
		return nil, fmt.Errorf("expected CIE ID 0, got 0x%x (this is an FDE)", cieID)
	}

	// Version.
	version, err := body.readU8()
	if err != nil {
		return nil, err
	}
	if version != 1 && version != 3 {
		return nil, fmt.Errorf("unsupported CIE version %d", version)
	}

	// Augmentation string (null-terminated).
	var augStr []byte
	for {
		b, err := body.readU8()
		if err != nil {
			return nil, fmt.Errorf("reading augmentation string: %w", err)
		}
		if b == 0 {
			break
		}
		augStr = append(augStr, b)
	}

	// Code alignment factor.
	cie.CodeAlignmentFactor, err = body.readULEB128()
	if err != nil {
		return nil, fmt.Errorf("reading code alignment factor: %w", err)
	}

	// Data alignment factor.
	cie.DataAlignmentFactor, err = body.readSLEB128()
	if err != nil {
		return nil, fmt.Errorf("reading data alignment factor: %w", err)
	}

	// Return address register.
	if version == 1 {
		ra, err := body.readU8()
		if err != nil {
			return nil, fmt.Errorf("reading return address register: %w", err)
		}
		cie.ReturnAddressRegister = uint64(ra)
	} else {
		cie.ReturnAddressRegister, err = body.readULEB128()
		if err != nil {
			return nil, fmt.Errorf("reading return address register: %w", err)
		}
	}

	// Default FDE pointer encoding to absolute if no augmentation overrides it.
	cie.FDEPointerEncoding = dwPtrAbsptr

	// Parse augmentation data if present.
	if len(augStr) > 0 && augStr[0] == 'z' {
		cie.HasAugmentation = true

		// Augmentation data length.
		augLen, err := body.readULEB128()
		if err != nil {
			return nil, fmt.Errorf("reading augmentation data length: %w", err)
		}
		augEnd := body.pos + int(augLen)

		// Process each augmentation character after 'z'.
		for _, c := range augStr[1:] {
			switch c {
			case 'R':
				// FDE pointer encoding.
				enc, err := body.readU8()
				if err != nil {
					return nil, fmt.Errorf("reading FDE pointer encoding: %w", err)
				}
				cie.FDEPointerEncoding = enc

			case 'L':
				// LSDA pointer encoding — read and store but we don't use it.
				enc, err := body.readU8()
				if err != nil {
					return nil, fmt.Errorf("reading LSDA encoding: %w", err)
				}
				cie.LSDAPointerEncoding = enc

			case 'P':
				// Personality routine — encoding byte + pointer. Skip it.
				pEnc, err := body.readU8()
				if err != nil {
					return nil, fmt.Errorf("reading personality encoding: %w", err)
				}
				if _, err := body.readAddress(pEnc); err != nil {
					return nil, fmt.Errorf("reading personality pointer: %w", err)
				}

			case 'S':
				// Signal frame — no data, just a flag. Ignore.

			default:
				// Unknown augmentation character — skip to end of augmentation data.
				body.pos = augEnd
			}
		}

		// Ensure we're at the end of the augmentation data.
		body.pos = augEnd
	}

	// Remaining bytes are the initial instructions.
	if body.remaining() > 0 {
		cie.Instructions = make([]byte, body.remaining())
		copy(cie.Instructions, body.data[body.pos:])
	}

	return cie, nil
}

// parseFDE reads an FDE at the given .eh_frame offset. The cieOffset
// is the absolute offset within .eh_frame where the referenced CIE starts.
func (cfi *CallFrameInformation) parseFDE(r *cfiReader, cieOffset uint64) (*FDE, error) {
	fde := &FDE{}

	// Look up the CIE.
	cie, err := cfi.getCIE(cieOffset)
	if err != nil {
		return nil, fmt.Errorf("FDE references CIE at offset %d: %w", cieOffset, err)
	}
	fde.CIE = cie

	// Initial location — encoded using the CIE's FDE pointer encoding.
	fde.InitialLocation, err = r.readAddress(cie.FDEPointerEncoding)
	if err != nil {
		return nil, fmt.Errorf("reading FDE initial location: %w", err)
	}

	// Address range — same format but always absolute (no relativity).
	fde.AddressRange, err = r.readAddress(cie.FDEPointerEncoding & 0x0f)
	if err != nil {
		return nil, fmt.Errorf("reading FDE address range: %w", err)
	}

	// If the CIE has augmentation, skip the FDE's augmentation data.
	if cie.HasAugmentation {
		augLen, err := r.readULEB128()
		if err != nil {
			return nil, fmt.Errorf("reading FDE augmentation length: %w", err)
		}
		r.pos += int(augLen)
	}

	// Remaining bytes are CFI instructions.
	if r.remaining() > 0 {
		fde.Instructions = make([]byte, r.remaining())
		copy(fde.Instructions, r.data[r.pos:])
	}

	return fde, nil
}

// parseFDEAtOffset reads a complete entry (length + CIE pointer + body)
// at the given .eh_frame offset and returns the parsed FDE.
func (cfi *CallFrameInformation) parseFDEAtOffset(offset uint64) (*FDE, error) {
	if int(offset) >= len(cfi.ehFrameData) {
		return nil, fmt.Errorf("FDE offset %d out of range", offset)
	}

	r := &cfiReader{
		data:     cfi.ehFrameData[offset:],
		baseAddr: cfi.ehFrameAddr + offset,
	}

	length, err := r.readU32()
	if err != nil {
		return nil, err
	}
	if length == 0 || length == 0xffffffff {
		return nil, fmt.Errorf("invalid FDE length %d at offset %d", length, offset)
	}

	// Create sub-reader for the entry body.
	entryStart := r.pos
	body, err := r.slice(int(length))
	if err != nil {
		return nil, err
	}

	// CIE pointer — in .eh_frame this is a backward offset from the
	// CIE pointer field itself to the start of the referenced CIE.
	ciePtr, err := body.readU32()
	if err != nil {
		return nil, err
	}
	if ciePtr == 0 {
		return nil, fmt.Errorf("expected FDE at offset %d, got CIE (CIE pointer is 0)", offset)
	}

	// The CIE pointer is a backward offset from the field that contains it.
	// Field position in .eh_frame = offset + entryStart (after length field).
	ciePtrFieldPos := offset + uint64(entryStart)
	cieOffset := ciePtrFieldPos - uint64(ciePtr)

	fde, err := cfi.parseFDE(body, cieOffset)
	if err != nil {
		return nil, err
	}
	fde.Length = length
	return fde, nil
}

// FDEForPC finds the FDE covering the given virtual address (runtime PC).
// It converts the PC to a file address by subtracting load bias, then
// uses the .eh_frame_hdr binary search table if available, otherwise
// falls back to a linear scan.
func (cfi *CallFrameInformation) FDEForPC(pc uint64) (*FDE, error) {
	// Convert runtime PC to file address (subtract ASLR bias).
	fileAddr := pc - cfi.loadBias
	if cfi.hdrHasTable {
		return cfi.fdeForPCBinarySearch(fileAddr)
	}
	return cfi.fdeForPCLinear(fileAddr)
}

// fdeForPCBinarySearch uses the .eh_frame_hdr sorted table.
// Each table entry is a pair: (initial_location, fde_pointer), both
// encoded with hdrTableEnc. The table is sorted by initial_location.
func (cfi *CallFrameInformation) fdeForPCBinarySearch(fileAddr uint64) (*FDE, error) {
	count := int(cfi.hdrTableCount)
	enc := cfi.hdrTableEnc

	// Compute entry size based on encoding format.
	entrySize := encodedPtrSize(enc)
	if entrySize == 0 {
		return nil, fmt.Errorf("cannot determine entry size for encoding 0x%02x", enc)
	}
	pairSize := entrySize * 2

	// Binary search: find the last entry where initial_location <= pc.
	idx := sort.Search(count, func(i int) bool {
		off := cfi.hdrTableOff + i*pairSize
		r := &cfiReader{
			data:     cfi.hdrData,
			baseAddr: cfi.hdrAddr,
			pos:      off,
		}
		loc, err := r.readAddress(enc)
		if err != nil {
			return true // treat errors as "too big"
		}
		return loc > fileAddr
	})
	idx-- // back up to last entry with initial_location <= pc

	if idx < 0 {
		return nil, fmt.Errorf("no FDE found for PC 0x%x (before all entries)", fileAddr)
	}

	// Read the matching entry's FDE pointer.
	off := cfi.hdrTableOff + idx*pairSize
	r := &cfiReader{
		data:     cfi.hdrData,
		baseAddr: cfi.hdrAddr,
		pos:      off,
	}
	// Skip initial_location.
	if _, err := r.readAddress(enc); err != nil {
		return nil, fmt.Errorf("reading table initial_location: %w", err)
	}
	// Read FDE pointer.
	fdeAddr, err := r.readAddress(enc)
	if err != nil {
		return nil, fmt.Errorf("reading table FDE pointer: %w", err)
	}

	// Convert FDE address to .eh_frame offset.
	fdeOffset := fdeAddr - cfi.ehFrameAddr
	fde, err := cfi.parseFDEAtOffset(fdeOffset)
	if err != nil {
		return nil, err
	}

	// Verify the file address is actually within this FDE's range.
	if !fde.ContainsPC(fileAddr) {
		return nil, fmt.Errorf("no FDE found for PC 0x%x (nearest FDE covers [0x%x, 0x%x))",
			fileAddr, fde.InitialLocation, fde.InitialLocation+fde.AddressRange)
	}

	return fde, nil
}

// fdeForPCLinear walks .eh_frame sequentially, parsing each entry to
// find the FDE containing the target address. Used as fallback when
// .eh_frame_hdr is not available.
func (cfi *CallFrameInformation) fdeForPCLinear(fileAddr uint64) (*FDE, error) {
	r := &cfiReader{
		data:     cfi.ehFrameData,
		baseAddr: cfi.ehFrameAddr,
	}

	for r.remaining() > 0 {
		entryOffset := uint64(r.pos)
		length, err := r.readU32()
		if err != nil {
			return nil, err
		}
		if length == 0 {
			break // terminator
		}
		if length == 0xffffffff {
			return nil, fmt.Errorf("64-bit DWARF not supported")
		}

		entryStart := r.pos
		body, err := r.slice(int(length))
		if err != nil {
			return nil, err
		}

		// Read the CIE ID / CIE pointer field.
		cieField, err := body.readU32()
		if err != nil {
			continue
		}

		if cieField == 0 {
			// This is a CIE — cache it for later FDE references.
			cieR := &cfiReader{
				data:     cfi.ehFrameData[entryOffset:],
				baseAddr: cfi.ehFrameAddr + entryOffset,
			}
			if cie, err := parseCIE(cieR); err == nil {
				cfi.cieCache[entryOffset] = cie
			}
			continue
		}

		// This is an FDE. Compute CIE offset.
		ciePtrFieldPos := entryOffset + uint64(entryStart)
		cieOffset := ciePtrFieldPos - uint64(cieField)

		fde, err := cfi.parseFDE(body, cieOffset)
		if err != nil {
			continue
		}
		fde.Length = length

		if fde.ContainsPC(fileAddr) {
			return fde, nil
		}
	}

	return nil, fmt.Errorf("no FDE found for file address 0x%x", fileAddr)
}

// encodedPtrSize returns the byte size of a pointer encoded with the
// given format nibble, or 0 if the size is variable (e.g., LEB128).
func encodedPtrSize(enc uint8) int {
	switch enc & 0x0f {
	case dwPtrAbsptr:
		return 8 // native pointer size on x86-64
	case dwPtrUdata2, dwPtrSdata2:
		return 2
	case dwPtrUdata4, dwPtrSdata4:
		return 4
	case dwPtrUdata8, dwPtrSdata8:
		return 8
	default:
		return 0
	}
}
