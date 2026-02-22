package debugger

// RegisterID uniquely identifies a register. It corresponds to the index
// of the register in the registerInfos slice.
type RegisterID int

// RegisterType classifies registers into categories.
type RegisterType int

const (
	RegisterTypeGPR    RegisterType = iota // 64-bit general-purpose register
	RegisterTypeSubGPR                     // Sub-register of a GPR (32/16/8-bit view)
	RegisterTypeFPR                        // Floating-point or vector register
	RegisterTypeDR                         // Debug register
)

// RegisterFormat describes how to interpret a register's value.
type RegisterFormat int

const (
	RegisterFormatUint        RegisterFormat = iota // Unsigned integer
	RegisterFormatDoubleFloat                       // 64-bit IEEE 754 double
	RegisterFormatLongDouble                        // 80-bit x87 extended precision (stored as 16 bytes)
	RegisterFormatVector                            // Opaque byte vector (SIMD)
)

// RegisterInfo describes a single register: its identity, name, DWARF
// number (from the SYSV ABI), byte size, byte offset inside the Linux
// struct user, category, and default display format.
type RegisterInfo struct {
	ID      RegisterID
	Name    string
	DwarfID int
	Size    int
	Offset  int
	Type    RegisterType
	Format  RegisterFormat
}

// -------------------------------------------------------------------------
// Offset constants for the Linux struct user layout (x86-64).
//
// struct user {
//     user_regs_struct  regs;          // offset   0, size 216
//     int               u_fpvalid;     // offset 216, size   4
//     // 4 bytes padding
//     user_fpregs_struct i387;         // offset 224, size 512
//     // misc fields ...
//     uint64            u_debugreg[8]; // offset 848, size  64
// };
//
// user_regs_struct field order (each uint64, 8 bytes):
//   r15(0) r14(8) r13(16) r12(24) rbp(32) rbx(40) r11(48) r10(56)
//   r9(64) r8(72) rax(80) rcx(88) rdx(96) rsi(104) rdi(112)
//   orig_rax(120) rip(128) cs(136) eflags(144) rsp(152) ss(160)
//   fs_base(168) gs_base(176) ds(184) es(192) fs(200) gs(208)
//
// user_fpregs_struct layout (starting at offset 224 in struct user):
//   cwd(+0,2) swd(+2,2) ftw(+4,2) fop(+6,2) rip(+8,8) rdp(+16,8)
//   mxcsr(+24,4) mxcr_mask(+28,4)
//   st_space[32 uint32](+32,128)   — 8 ST/MM regs, 16 bytes each
//   xmm_space[64 uint32](+160,256) — 16 XMM regs, 16 bytes each
//   padding[24 uint32](+416,96)
// -------------------------------------------------------------------------

const (
	// GPR offsets within struct user (= offsets within user_regs_struct).
	offR15     = 0
	offR14     = 8
	offR13     = 16
	offR12     = 24
	offRbp     = 32
	offRbx     = 40
	offR11     = 48
	offR10     = 56
	offR9      = 64
	offR8      = 72
	offRax     = 80
	offRcx     = 88
	offRdx     = 96
	offRsi     = 104
	offRdi     = 112
	offOrigRax = 120
	offRip     = 128
	offCs      = 136
	offEflags  = 144
	offRsp     = 152
	offSs      = 160
	offFsBase  = 168
	offGsBase  = 176
	offDs      = 184
	offEs      = 192
	offFs      = 200
	offGs      = 208

	// FPR base offset (user.i387).
	offFPR = 224

	// FPR field offsets relative to user.i387.
	fprCwd      = offFPR + 0
	fprSwd      = offFPR + 2
	fprFtw      = offFPR + 4
	fprFop      = offFPR + 6
	fprRip      = offFPR + 8
	fprRdp      = offFPR + 16
	fprMxcsr    = offFPR + 24
	fprMxcrMask = offFPR + 28
	fprStSpace  = offFPR + 32  // st_space[32] — 128 bytes
	fprXmmSpace = offFPR + 160 // xmm_space[64] — 256 bytes

	// Debug register base offset (user.u_debugreg).
	offDebugReg = 848
)

// registerInfos is the single source of truth for all x86-64 registers.
// The slice index is the RegisterID. An init function sets each entry's
// ID field automatically.
//
// Register count: 25 64-bit GPRs + 16 32-bit sub + 16 16-bit sub
//   - 4 high-8 sub + 16 low-8 sub + 8 FP control/status
//   - 8 ST + 8 MM + 16 XMM + 8 DR = 125 registers.
var registerInfos = []RegisterInfo{
	// ── 64-bit GPRs ────────────────────────────────────────────────
	// DWARF IDs from the SYSV ABI x86-64 supplement.
	{Name: "rax", DwarfID: 0, Size: 8, Offset: offRax, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "rdx", DwarfID: 1, Size: 8, Offset: offRdx, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "rcx", DwarfID: 2, Size: 8, Offset: offRcx, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "rbx", DwarfID: 3, Size: 8, Offset: offRbx, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "rsi", DwarfID: 4, Size: 8, Offset: offRsi, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "rdi", DwarfID: 5, Size: 8, Offset: offRdi, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "rbp", DwarfID: 6, Size: 8, Offset: offRbp, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "rsp", DwarfID: 7, Size: 8, Offset: offRsp, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "r8", DwarfID: 8, Size: 8, Offset: offR8, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "r9", DwarfID: 9, Size: 8, Offset: offR9, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "r10", DwarfID: 10, Size: 8, Offset: offR10, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "r11", DwarfID: 11, Size: 8, Offset: offR11, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "r12", DwarfID: 12, Size: 8, Offset: offR12, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "r13", DwarfID: 13, Size: 8, Offset: offR13, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "r14", DwarfID: 14, Size: 8, Offset: offR14, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "r15", DwarfID: 15, Size: 8, Offset: offR15, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "rip", DwarfID: 16, Size: 8, Offset: offRip, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "eflags", DwarfID: 49, Size: 8, Offset: offEflags, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "cs", DwarfID: 51, Size: 8, Offset: offCs, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "fs", DwarfID: 54, Size: 8, Offset: offFs, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "gs", DwarfID: 55, Size: 8, Offset: offGs, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "ss", DwarfID: 52, Size: 8, Offset: offSs, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "ds", DwarfID: 53, Size: 8, Offset: offDs, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	{Name: "es", DwarfID: 50, Size: 8, Offset: offEs, Type: RegisterTypeGPR, Format: RegisterFormatUint},
	// orig_rax: ptrace provides this to identify syscalls. No DWARF ID.
	{Name: "orig_rax", DwarfID: -1, Size: 8, Offset: offOrigRax, Type: RegisterTypeGPR, Format: RegisterFormatUint},

	// ── 32-bit sub-GPRs ────────────────────────────────────────────
	// On little-endian x64, the low 32 bits live at the same offset.
	{Name: "eax", DwarfID: -1, Size: 4, Offset: offRax, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "edx", DwarfID: -1, Size: 4, Offset: offRdx, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "ecx", DwarfID: -1, Size: 4, Offset: offRcx, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "ebx", DwarfID: -1, Size: 4, Offset: offRbx, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "esi", DwarfID: -1, Size: 4, Offset: offRsi, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "edi", DwarfID: -1, Size: 4, Offset: offRdi, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "ebp", DwarfID: -1, Size: 4, Offset: offRbp, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "esp", DwarfID: -1, Size: 4, Offset: offRsp, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r8d", DwarfID: -1, Size: 4, Offset: offR8, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r9d", DwarfID: -1, Size: 4, Offset: offR9, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r10d", DwarfID: -1, Size: 4, Offset: offR10, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r11d", DwarfID: -1, Size: 4, Offset: offR11, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r12d", DwarfID: -1, Size: 4, Offset: offR12, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r13d", DwarfID: -1, Size: 4, Offset: offR13, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r14d", DwarfID: -1, Size: 4, Offset: offR14, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r15d", DwarfID: -1, Size: 4, Offset: offR15, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},

	// ── 16-bit sub-GPRs ────────────────────────────────────────────
	{Name: "ax", DwarfID: -1, Size: 2, Offset: offRax, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "dx", DwarfID: -1, Size: 2, Offset: offRdx, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "cx", DwarfID: -1, Size: 2, Offset: offRcx, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "bx", DwarfID: -1, Size: 2, Offset: offRbx, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "si", DwarfID: -1, Size: 2, Offset: offRsi, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "di", DwarfID: -1, Size: 2, Offset: offRdi, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "bp", DwarfID: -1, Size: 2, Offset: offRbp, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "sp", DwarfID: -1, Size: 2, Offset: offRsp, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r8w", DwarfID: -1, Size: 2, Offset: offR8, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r9w", DwarfID: -1, Size: 2, Offset: offR9, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r10w", DwarfID: -1, Size: 2, Offset: offR10, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r11w", DwarfID: -1, Size: 2, Offset: offR11, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r12w", DwarfID: -1, Size: 2, Offset: offR12, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r13w", DwarfID: -1, Size: 2, Offset: offR13, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r14w", DwarfID: -1, Size: 2, Offset: offR14, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r15w", DwarfID: -1, Size: 2, Offset: offR15, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},

	// ── High 8-bit sub-GPRs (a, b, c, d only) ─────────────────────
	// The high byte of the 16-bit register lives at offset+1 on little-endian.
	{Name: "ah", DwarfID: -1, Size: 1, Offset: offRax + 1, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "bh", DwarfID: -1, Size: 1, Offset: offRbx + 1, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "ch", DwarfID: -1, Size: 1, Offset: offRcx + 1, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "dh", DwarfID: -1, Size: 1, Offset: offRdx + 1, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},

	// ── Low 8-bit sub-GPRs ─────────────────────────────────────────
	{Name: "al", DwarfID: -1, Size: 1, Offset: offRax, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "bl", DwarfID: -1, Size: 1, Offset: offRbx, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "cl", DwarfID: -1, Size: 1, Offset: offRcx, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "dl", DwarfID: -1, Size: 1, Offset: offRdx, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "sil", DwarfID: -1, Size: 1, Offset: offRsi, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "dil", DwarfID: -1, Size: 1, Offset: offRdi, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "bpl", DwarfID: -1, Size: 1, Offset: offRbp, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "spl", DwarfID: -1, Size: 1, Offset: offRsp, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r8b", DwarfID: -1, Size: 1, Offset: offR8, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r9b", DwarfID: -1, Size: 1, Offset: offR9, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r10b", DwarfID: -1, Size: 1, Offset: offR10, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r11b", DwarfID: -1, Size: 1, Offset: offR11, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r12b", DwarfID: -1, Size: 1, Offset: offR12, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r13b", DwarfID: -1, Size: 1, Offset: offR13, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r14b", DwarfID: -1, Size: 1, Offset: offR14, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},
	{Name: "r15b", DwarfID: -1, Size: 1, Offset: offR15, Type: RegisterTypeSubGPR, Format: RegisterFormatUint},

	// ── FP control/status registers ────────────────────────────────
	{Name: "fcw", DwarfID: 65, Size: 2, Offset: fprCwd, Type: RegisterTypeFPR, Format: RegisterFormatUint},
	{Name: "fsw", DwarfID: 66, Size: 2, Offset: fprSwd, Type: RegisterTypeFPR, Format: RegisterFormatUint},
	{Name: "ftw", DwarfID: -1, Size: 2, Offset: fprFtw, Type: RegisterTypeFPR, Format: RegisterFormatUint},
	{Name: "fop", DwarfID: -1, Size: 2, Offset: fprFop, Type: RegisterTypeFPR, Format: RegisterFormatUint},
	{Name: "frip", DwarfID: -1, Size: 8, Offset: fprRip, Type: RegisterTypeFPR, Format: RegisterFormatUint},
	{Name: "frdp", DwarfID: -1, Size: 8, Offset: fprRdp, Type: RegisterTypeFPR, Format: RegisterFormatUint},
	{Name: "mxcsr", DwarfID: 64, Size: 4, Offset: fprMxcsr, Type: RegisterTypeFPR, Format: RegisterFormatUint},
	{Name: "mxcsrmask", DwarfID: -1, Size: 4, Offset: fprMxcrMask, Type: RegisterTypeFPR, Format: RegisterFormatUint},

	// ── ST registers (x87 floating-point stack) ────────────────────
	// 80-bit precision, stored as 16 bytes in user_fpregs_struct.
	{Name: "st0", DwarfID: 33, Size: 16, Offset: fprStSpace + 0*16, Type: RegisterTypeFPR, Format: RegisterFormatLongDouble},
	{Name: "st1", DwarfID: 34, Size: 16, Offset: fprStSpace + 1*16, Type: RegisterTypeFPR, Format: RegisterFormatLongDouble},
	{Name: "st2", DwarfID: 35, Size: 16, Offset: fprStSpace + 2*16, Type: RegisterTypeFPR, Format: RegisterFormatLongDouble},
	{Name: "st3", DwarfID: 36, Size: 16, Offset: fprStSpace + 3*16, Type: RegisterTypeFPR, Format: RegisterFormatLongDouble},
	{Name: "st4", DwarfID: 37, Size: 16, Offset: fprStSpace + 4*16, Type: RegisterTypeFPR, Format: RegisterFormatLongDouble},
	{Name: "st5", DwarfID: 38, Size: 16, Offset: fprStSpace + 5*16, Type: RegisterTypeFPR, Format: RegisterFormatLongDouble},
	{Name: "st6", DwarfID: 39, Size: 16, Offset: fprStSpace + 6*16, Type: RegisterTypeFPR, Format: RegisterFormatLongDouble},
	{Name: "st7", DwarfID: 40, Size: 16, Offset: fprStSpace + 7*16, Type: RegisterTypeFPR, Format: RegisterFormatLongDouble},

	// ── MM registers (MMX, alias the ST register storage) ──────────
	// 64-bit, but stored with 8 bytes of padding (16-byte stride).
	{Name: "mm0", DwarfID: 41, Size: 8, Offset: fprStSpace + 0*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "mm1", DwarfID: 42, Size: 8, Offset: fprStSpace + 1*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "mm2", DwarfID: 43, Size: 8, Offset: fprStSpace + 2*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "mm3", DwarfID: 44, Size: 8, Offset: fprStSpace + 3*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "mm4", DwarfID: 45, Size: 8, Offset: fprStSpace + 4*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "mm5", DwarfID: 46, Size: 8, Offset: fprStSpace + 5*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "mm6", DwarfID: 47, Size: 8, Offset: fprStSpace + 6*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "mm7", DwarfID: 48, Size: 8, Offset: fprStSpace + 7*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},

	// ── XMM registers (SSE/SSE2) ───────────────────────────────────
	// 128-bit vector registers.
	{Name: "xmm0", DwarfID: 17, Size: 16, Offset: fprXmmSpace + 0*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "xmm1", DwarfID: 18, Size: 16, Offset: fprXmmSpace + 1*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "xmm2", DwarfID: 19, Size: 16, Offset: fprXmmSpace + 2*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "xmm3", DwarfID: 20, Size: 16, Offset: fprXmmSpace + 3*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "xmm4", DwarfID: 21, Size: 16, Offset: fprXmmSpace + 4*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "xmm5", DwarfID: 22, Size: 16, Offset: fprXmmSpace + 5*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "xmm6", DwarfID: 23, Size: 16, Offset: fprXmmSpace + 6*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "xmm7", DwarfID: 24, Size: 16, Offset: fprXmmSpace + 7*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "xmm8", DwarfID: 25, Size: 16, Offset: fprXmmSpace + 8*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "xmm9", DwarfID: 26, Size: 16, Offset: fprXmmSpace + 9*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "xmm10", DwarfID: 27, Size: 16, Offset: fprXmmSpace + 10*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "xmm11", DwarfID: 28, Size: 16, Offset: fprXmmSpace + 11*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "xmm12", DwarfID: 29, Size: 16, Offset: fprXmmSpace + 12*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "xmm13", DwarfID: 30, Size: 16, Offset: fprXmmSpace + 13*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "xmm14", DwarfID: 31, Size: 16, Offset: fprXmmSpace + 14*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},
	{Name: "xmm15", DwarfID: 32, Size: 16, Offset: fprXmmSpace + 15*16, Type: RegisterTypeFPR, Format: RegisterFormatVector},

	// ── Debug registers ────────────────────────────────────────────
	{Name: "dr0", DwarfID: -1, Size: 8, Offset: offDebugReg + 0*8, Type: RegisterTypeDR, Format: RegisterFormatUint},
	{Name: "dr1", DwarfID: -1, Size: 8, Offset: offDebugReg + 1*8, Type: RegisterTypeDR, Format: RegisterFormatUint},
	{Name: "dr2", DwarfID: -1, Size: 8, Offset: offDebugReg + 2*8, Type: RegisterTypeDR, Format: RegisterFormatUint},
	{Name: "dr3", DwarfID: -1, Size: 8, Offset: offDebugReg + 3*8, Type: RegisterTypeDR, Format: RegisterFormatUint},
	{Name: "dr4", DwarfID: -1, Size: 8, Offset: offDebugReg + 4*8, Type: RegisterTypeDR, Format: RegisterFormatUint},
	{Name: "dr5", DwarfID: -1, Size: 8, Offset: offDebugReg + 5*8, Type: RegisterTypeDR, Format: RegisterFormatUint},
	{Name: "dr6", DwarfID: -1, Size: 8, Offset: offDebugReg + 6*8, Type: RegisterTypeDR, Format: RegisterFormatUint},
	{Name: "dr7", DwarfID: -1, Size: 8, Offset: offDebugReg + 7*8, Type: RegisterTypeDR, Format: RegisterFormatUint},
}

func init() {
	for i := range registerInfos {
		registerInfos[i].ID = RegisterID(i)
	}
}

// AllRegisterInfos returns a copy of the complete register metadata table.
func AllRegisterInfos() []RegisterInfo {
	out := make([]RegisterInfo, len(registerInfos))
	copy(out, registerInfos)
	return out
}

// RegisterInfoByID returns the register info for the given ID.
func RegisterInfoByID(id RegisterID) (RegisterInfo, bool) {
	i := int(id)
	if i < 0 || i >= len(registerInfos) {
		return RegisterInfo{}, false
	}
	return registerInfos[i], true
}

// RegisterInfoByName returns the register info with the given name.
func RegisterInfoByName(name string) (RegisterInfo, bool) {
	for _, info := range registerInfos {
		if info.Name == name {
			return info, true
		}
	}
	return RegisterInfo{}, false
}

// RegisterInfoByDwarf returns the register info with the given DWARF ID.
// DWARF IDs of -1 are ignored (they indicate "no DWARF mapping").
func RegisterInfoByDwarf(dwarfID int) (RegisterInfo, bool) {
	if dwarfID < 0 {
		return RegisterInfo{}, false
	}
	for _, info := range registerInfos {
		if info.DwarfID == dwarfID {
			return info, true
		}
	}
	return RegisterInfo{}, false
}
