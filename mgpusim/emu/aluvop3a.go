package emu

import (
	"log"
	"math"
	"sort"
	"strings"

	"gitlab.com/akita/mgpusim/bitops"
)

//nolint:gocyclo,funlen
func (u *ALUImpl) runVOP3A(state InstEmuState) {
	inst := state.Inst()

	u.vop3aPreprocess(state)

	switch inst.Opcode {
	case 16: //0x10
		u.runVCmpClassF32VOP3a(state)
	case 65: // 0x41
		u.runVCmpLtF32VOP3a(state)
	case 66: // 0x42
		u.runVCmpEqF32VOP3a(state)
	case 68: //0x44
		u.runVCmpGtF32VOP3a(state)
	case 75: // 0x4B
		u.runVCmpNgtF32VOP3a(state)
	case 77: // 0x4D v_cmp_neq_f32
		u.runVCmpNeqF32VOP3a(state)
	case 78: // 0x41
		u.runVCmpNltF32VOP3a(state)
	case 193: // 0xC1
		u.runVCmpLtI32VOP3a(state)
	case 195: // 0xC3
		u.runVCmpLeI32VOP3a(state)
	case 196: // 0xC4
		u.runVCmpGtI32VOP3a(state)
	case 198: // 0xC6
		u.runVCmpGEI32VOP3a(state)
	case 201: // 0xC9
		u.runVCmpLtU32VOP3a(state)
	case 170: // 0xAA
		u.runVCmpEqU16VOP3a(state)
	case 202: // 0xCA
		u.runVCmpEqU32VOP3a(state)
	case 203: // 0xCB
		u.runVCmpLeU32VOP3a(state)
	case 204: // 0xCC
		u.runVCmpGtU32VOP3a(state)
	case 205: // 0xCD
		u.runVCmpLgU32VOP3a(state)
	case 206: // 0xCE
		u.runVCmpGeU32VOP3a(state)
	case 233: // 0xE9
		u.runVCmpLtU64VOP3a(state)
	case 238: //V_CMP_GE_U64
		u.runVCmpGeU64VOP3a(state)
	case 256:
		u.runVCNDMASKB32VOP3a(state)
	case 258:
		u.runVSUBF32VOP3a(state)
	case 261:
		u.runVMULF32VOP3a(state)
	case 278:
		u.runVMACF32VOP3a(state)
	case 353: // v_log_f32 (VOP1 0x21) in VOP3a encoding
		u.runVLOGF32VOP3a(state)
	case 354: // v_rcp_f32 (VOP1 0x22) in VOP3a encoding
		u.runVRCPF32VOP3a(state)
	case 449:
		u.runVMADF32(state)
	case 450:
		u.runVMADI32I24(state)
	case 451, 488:
		u.runVMADU64U32(state)
	case 489, 497:
		// v_mad_i64_i32: signed dst64 = sext(src0_i32) * sext(src1_i32) + src2_i64.
		// LLVM emits opcode 489 (gfx8 legacy) or 497 (gfx9 / newer LLVM
		// back-mapped to gfx8); accept both. Clang-ocl uses this when
		// lowering 32-bit-index pointer arithmetic on 64-bit address space
		// (e.g. base + index * stride + offset in struct indexing).
		u.runVMADI64I32(state)
	case 456:
		u.runVBFEU32(state)
	case 457:
		u.runVBFEI32(state)
	case 459: // 0x1CB v_fma_f32
		u.runVFMAF32(state)
	case 460:
		u.runVFMAF64(state)
	case 462:
		u.runVALIGNBITB32(state)
	case 464:
		u.runVMIN3F32(state)
	case 465:
		u.runVMIN3I32(state)
	case 466:
		u.runVMIN3U32(state)
	case 467:
		u.runVMAX3F32(state)
	case 468:
		u.runVMAX3I32(state)
	case 469:
		u.runVMAX3U32(state)
	case 470:
		u.runVMED3F32(state)
	case 471:
		u.runVMED3I32(state)
	case 472:
		u.runVMED3U32(state)
	case 479:
		u.runVDIVFIXUPF64(state)
	case 483:
		u.runVDIVFMASF64(state)
	case 640:
		u.runVADDF64(state)
	case 641:
		u.runVMULF64(state)
	case 645:
		u.runVMULLOU32(state)
	case 646:
		u.runVMULHIU32(state)
	case 647:
		u.runVMULHII32(state)
	case 649:
		u.runVREADLANEB32(state)
	case 650:
		u.runVWRITELANEB32(state)
	case 655:
		u.runVLSHLREVB64(state)
	case 656:
		u.runVLSHRREVB64(state)
	case 657:
		u.runVASHRREVI64(state)
	default:
		log.Panicf("Opcode %d for VOP3a format is not implemented", inst.Opcode)
	}
	u.vop3aPostprocess(state)
}

func (u *ALUImpl) vop3aPreprocess(state InstEmuState) {
	inst := state.Inst()

	if inst.Abs != 0 {
		u.vop3aPreProcessAbs(state)
	}

	if inst.Neg != 0 {
		u.vop3aPreProcessNeg(state)
	}
}

func (u *ALUImpl) vop3aPreProcessAbs(state InstEmuState) {
	inst := state.Inst()
	sp := state.Scratchpad().AsVOP3A()

	if strings.Contains(inst.InstName, "F32") ||
		strings.Contains(inst.InstName, "f32") {
		if inst.Abs&0x1 != 0 {
			for i := 0; i < 64; i++ {
				src0 := math.Float32frombits(uint32(sp.SRC0[i]))
				src0 = float32(math.Abs(float64(src0)))
				sp.SRC0[i] = uint64(math.Float32bits(src0))
			}
		}

		if inst.Abs&0x2 != 0 {
			for i := 0; i < 64; i++ {
				src1 := math.Float32frombits(uint32(sp.SRC1[i]))
				src1 = float32(math.Abs(float64(src1)))
				sp.SRC1[i] = uint64(math.Float32bits(src1))
			}
		}

		if inst.Abs&0x4 != 0 {
			for i := 0; i < 64; i++ {
				src2 := math.Float32frombits(uint32(sp.SRC2[i]))
				src2 = float32(math.Abs(float64(src2)))
				sp.SRC2[i] = uint64(math.Float32bits(src2))
			}
		}
	} else {
		log.Printf("Absolute operation for %s is not implemented.", inst.InstName)
	}
}

func (u *ALUImpl) vop3aPreProcessNeg(state InstEmuState) {
	inst := state.Inst()

	if strings.Contains(inst.InstName, "F64") ||
		strings.Contains(inst.InstName, "f64") {
		u.vop3aPreProcessF64Neg(state)
	} else if strings.Contains(inst.InstName, "F32") ||
		strings.Contains(inst.InstName, "f32") {
		u.vop3aPreProcessF32Neg(state)
	} else if strings.Contains(inst.InstName, "B32") ||
		strings.Contains(inst.InstName, "b32") {
		u.vop3aPreProcessB32Neg(state)
	} else {
		log.Printf("Negative operation for %s is not implemented.", inst.InstName)
	}
}

func (u *ALUImpl) vop3aPreProcessF64Neg(state InstEmuState) {
	inst := state.Inst()
	sp := state.Scratchpad().AsVOP3A()

	if inst.Neg&0x1 != 0 {
		for i := 0; i < 64; i++ {
			src0 := math.Float64frombits(sp.SRC0[i])
			src0 = src0 * (-1.0)
			sp.SRC0[i] = math.Float64bits(src0)
		}
	}

	if inst.Neg&0x2 != 0 {
		for i := 0; i < 64; i++ {
			src1 := math.Float64frombits(sp.SRC1[i])
			src1 = src1 * (-1.0)
			sp.SRC1[i] = math.Float64bits(src1)
		}
	}

	if inst.Neg&0x4 != 0 {
		for i := 0; i < 64; i++ {
			src2 := math.Float64frombits(sp.SRC2[i])
			src2 = src2 * (-1.0)
			sp.SRC2[i] = math.Float64bits(src2)
		}
	}
}

func (u *ALUImpl) vop3aPreProcessF32Neg(state InstEmuState) {
	inst := state.Inst()
	sp := state.Scratchpad().AsVOP3A()
	if inst.Neg&0x1 != 0 {
		for i := 0; i < 64; i++ {
			src0 := math.Float32frombits(uint32(sp.SRC0[i]))
			src0 = src0 * (-1.0)
			sp.SRC0[i] = uint64(math.Float32bits(src0))
		}
	}

	if inst.Neg&0x2 != 0 {
		for i := 0; i < 64; i++ {
			src1 := math.Float32frombits(uint32(sp.SRC1[i]))
			src1 = src1 * (-1.0)
			sp.SRC1[i] = uint64(math.Float32bits(src1))
		}
	}

	if inst.Neg&0x4 != 0 {
		for i := 0; i < 64; i++ {
			src2 := math.Float32frombits(uint32(sp.SRC2[i]))
			src2 = src2 * (-1.0)
			sp.SRC2[i] = uint64(math.Float32bits(src2))
		}
	}
}

func (u *ALUImpl) vop3aPreProcessB32Neg(state InstEmuState) {
	inst := state.Inst()
	sp := state.Scratchpad().AsVOP3A()
	if inst.Neg&0x1 != 0 {
		for i := 0; i < 64; i++ {
			src0 := asInt32(uint32(sp.SRC0[i]))
			src0 = src0 * (-1.0)
			sp.SRC0[i] = uint64(int32ToBits(src0))
		}
	}

	if inst.Neg&0x2 != 0 {
		for i := 0; i < 64; i++ {
			src1 := asInt32(uint32(sp.SRC1[i]))
			src1 = src1 * (-1.0)
			sp.SRC1[i] = uint64(int32ToBits(src1))
		}
	}

	if inst.Neg&0x4 != 0 {
		for i := 0; i < 64; i++ {
			src2 := asInt32(uint32(sp.SRC2[i]))
			src2 = src2 * (-1.0)
			sp.SRC2[i] = uint64(int32ToBits(src2))
		}
	}
}

func (u *ALUImpl) vop3aPostprocess(state InstEmuState) {
	inst := state.Inst()

	if inst.Omod != 0 {
		log.Panic("Output modifiers are not supported.")
	}
}

func (u *ALUImpl) runVCmpClassF32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	sp.VCC = 0
	var i uint
	var src0 float32
	var src1 uint32
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}
		src0 = math.Float32frombits(uint32(sp.SRC0[i]))
		src1 = uint32(sp.SRC1[i])

		if ((src1 >> 0) & 0x1) != 0 {
			if math.IsNaN(float64(src0)) {
				sp.VCC = sp.VCC | (1 << i)
			}
		}
		if ((src1 >> 2) & 0x1) != 0 {
			if math.IsInf(float64(src0), -1) {
				sp.VCC = sp.VCC | (1 << i)
			}
		}
		if ((src1 >> 6) & 0x1) != 0 {
			if src0 == 0.0 {
				sp.VCC = sp.VCC | (1 << i)
			}
		}
		if ((src1 >> 7) & 0x1) != 0 {
			if src0 == 0.0 {
				sp.VCC = sp.VCC | (1 << i)
			}
		}
		if ((src1 >> 9) & 0x1) != 0 {
			if math.IsInf(float64(src0), 1) {
				sp.VCC = sp.VCC | (1 << i)
			}
		}
	}
}

func (u *ALUImpl) runVCmpLtF32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	// sp.VCC = 0
	var i uint
	var src0, src1 float32
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}
		src0 = math.Float32frombits(uint32(sp.SRC0[i]))
		src1 = math.Float32frombits(uint32(sp.SRC1[i]))
		if src0 < src1 {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCmpGtF32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	// sp.VCC = 0
	var i uint
	var src0, src1 float32
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}
		src0 = math.Float32frombits(uint32(sp.SRC0[i]))
		src1 = math.Float32frombits(uint32(sp.SRC1[i]))
		if src0 > src1 {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCmpNltF32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	// sp.VCC = 0
	var i uint
	var src0, src1 float32
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}
		src0 = math.Float32frombits(uint32(sp.SRC0[i]))
		src1 = math.Float32frombits(uint32(sp.SRC1[i]))
		if !(src0 < src1) {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCmpEqF32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	var i uint
	var src0, src1 float32
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}
		src0 = math.Float32frombits(uint32(sp.SRC0[i]))
		src1 = math.Float32frombits(uint32(sp.SRC1[i]))
		if src0 == src1 {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCmpNeqF32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	var i uint
	var src0, src1 float32
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}
		src0 = math.Float32frombits(uint32(sp.SRC0[i]))
		src1 = math.Float32frombits(uint32(sp.SRC1[i]))
		if !(src0 == src1) {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCmpNgtF32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	var i uint
	var src0, src1 float32
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}
		src0 = math.Float32frombits(uint32(sp.SRC0[i]))
		src1 = math.Float32frombits(uint32(sp.SRC1[i]))
		if !(src0 > src1) {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCmpLtI32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src0 := asInt32(uint32(sp.SRC0[i]))
		src1 := asInt32(uint32(sp.SRC1[i]))

		if src0 < src1 {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCmpLeI32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src0 := asInt32(uint32(sp.SRC0[i]))
		src1 := asInt32(uint32(sp.SRC1[i]))

		if src0 <= src1 {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCmpGtI32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src0 := asInt32(uint32(sp.SRC0[i]))
		src1 := asInt32(uint32(sp.SRC1[i]))

		if src0 > src1 {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCmpGEI32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src0 := asInt32(uint32(sp.SRC0[i]))
		src1 := asInt32(uint32(sp.SRC1[i]))

		if src0 >= src1 {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCmpLtU32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src0 := sp.SRC0[i]
		src1 := sp.SRC1[i]

		if src0 < src1 {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCmpEqU32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src0 := sp.SRC0[i]
		src1 := sp.SRC1[i]

		if uint32(src0) == uint32(src1) {
			sp.DST[0] |= (1 << i)
		}
	}
}

// runVCmpEqU16VOP3a implements V_CMP_EQ_U16 (VOP3a op 170): per-lane
// 16-bit unsigned equality, result bit written into the SGPR mask.
func (u *ALUImpl) runVCmpEqU16VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		if uint16(sp.SRC0[i]) == uint16(sp.SRC1[i]) {
			sp.DST[0] |= (1 << i)
		}
	}
}

// runVBFEU32 implements V_BFE_U32 (VOP3a op 456): unsigned bitfield
// extract. D = (S0 >> S1[4:0]) & ((1 << S2[4:0]) - 1).
func (u *ALUImpl) runVBFEU32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src := uint32(sp.SRC0[i])
		offset := uint32(sp.SRC1[i]) & 0x1f
		width := uint32(sp.SRC2[i]) & 0x1f

		var res uint32
		if width != 0 {
			res = (src >> offset) & ((uint32(1) << width) - 1)
		}
		sp.DST[i] = uint64(res)
	}
}

// runVBFEI32 implements V_BFE_I32 (VOP3a op 457): signed bitfield
// extract, sign-extending the width-bit field.
func (u *ALUImpl) runVBFEI32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src := uint32(sp.SRC0[i])
		offset := uint32(sp.SRC1[i]) & 0x1f
		width := uint32(sp.SRC2[i]) & 0x1f

		var res int32
		if width != 0 {
			field := (src >> offset) & ((uint32(1) << width) - 1)
			shift := 32 - width
			res = int32(field<<shift) >> shift
		}
		sp.DST[i] = uint64(int32ToBits(res))
	}
}

// runVALIGNBITB32 implements V_ALIGNBIT_B32 (VOP3a op 462):
// D = ({S0,S1} >> S2[4:0])[31:0], with S0 the high word, S1 the low.
func (u *ALUImpl) runVALIGNBITB32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		hi := uint64(uint32(sp.SRC0[i]))
		lo := uint64(uint32(sp.SRC1[i]))
		shift := uint(sp.SRC2[i]) & 0x1f
		combined := (hi << 32) | lo
		sp.DST[i] = uint64(uint32(combined >> shift))
	}
}

func (u *ALUImpl) runVCmpLeU32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src0 := sp.SRC0[i]
		src1 := sp.SRC1[i]

		if src0 <= src1 {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCmpGtU32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src0 := sp.SRC0[i]
		src1 := sp.SRC1[i]

		if src0 > src1 {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCmpLgU32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src0 := sp.SRC0[i]
		src1 := sp.SRC1[i]

		if src0 != src1 {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCmpGeU32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src0 := sp.SRC0[i]
		src1 := sp.SRC1[i]

		if src0 >= src1 {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCmpLtU64VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src0 := sp.SRC0[i]
		src1 := sp.SRC1[i]

		if src0 < src1 {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCmpGeU64VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src0 := sp.SRC0[i]
		src1 := sp.SRC1[i]

		if src0 >= src1 {
			sp.DST[0] |= (1 << i)
		}
	}
}

func (u *ALUImpl) runVCNDMASKB32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		if (sp.SRC2[i] & (1 << i)) > 0 {
			sp.DST[i] = sp.SRC1[i]
		} else {
			sp.DST[i] = sp.SRC0[i]
		}
	}
}

func (u *ALUImpl) runVSUBF32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}
		src0 := math.Float32frombits(uint32(sp.SRC0[i]))
		src1 := math.Float32frombits(uint32(sp.SRC1[i]))
		dst := src0 - src1
		sp.DST[i] = uint64(math.Float32bits(dst))
	}
}
func (u *ALUImpl) runVMULF32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}
		src0 := math.Float32frombits(uint32(sp.SRC0[i]))
		src1 := math.Float32frombits(uint32(sp.SRC1[i]))
		dst := src0 * src1
		sp.DST[i] = uint64(math.Float32bits(dst))
	}
}

// runVLOGF32VOP3a implements V_LOG_F32 (log base 2) in VOP3a encoding
// (VOP1 opcode 0x21 promoted to VOP3, runtime opcode 353).
func (u *ALUImpl) runVLOGF32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}
		src0 := math.Float32frombits(uint32(sp.SRC0[i]))
		dst := float32(math.Log2(float64(src0)))
		sp.DST[i] = uint64(math.Float32bits(dst))
	}
}

// runVRCPF32VOP3a — v_rcp_f32 in VOP3a encoding (opcode 354).
// Mirrors VOP1 form (aluvop1.go case 34, runVRCPIFLAGF32), reading
// modifier-adjusted SRC0 prepared by vop3aPreprocess.
// clang-ocl emits this when a v_rcp_f32 needs a src input modifier
// (abs/neg) or when register pressure forces VOP3 encoding — e.g. the
// reciprocal step of native_divide(a, b) lowering.
func (u *ALUImpl) runVRCPF32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}
		src0 := math.Float32frombits(uint32(sp.SRC0[i]))
		dst := float32(1) / src0
		sp.DST[i] = uint64(math.Float32bits(dst))
	}
}

func (u *ALUImpl) runVMACF32VOP3a(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}
		src0 := math.Float32frombits(uint32(sp.SRC0[i]))
		src1 := math.Float32frombits(uint32(sp.SRC1[i]))
		dst := math.Float32frombits(uint32(sp.DST[i]))
		dst = src0*src1 + dst
		sp.DST[i] = uint64(math.Float32bits(dst))
	}
}

func (u *ALUImpl) runVMADF32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}
		src0 := math.Float32frombits(uint32(sp.SRC0[i]))
		src1 := math.Float32frombits(uint32(sp.SRC1[i]))
		src2 := math.Float32frombits(uint32(sp.SRC2[i]))

		res := src0*src1 + src2
		sp.DST[i] = uint64(math.Float32bits(res))
	}
}

// runVFMAF32 emulates v_fma_f32 (opcode 459). GCN3 v_fma is a fused
// multiply-add (single rounding) vs v_mad's two roundings; the emu mirrors
// v_mad_f32's src0*src1+src2 so the result matches the CPU float32 reference
// used by -verify. The ULP-level fused/non-fused difference is immaterial here.
func (u *ALUImpl) runVFMAF32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}
		src0 := math.Float32frombits(uint32(sp.SRC0[i]))
		src1 := math.Float32frombits(uint32(sp.SRC1[i]))
		src2 := math.Float32frombits(uint32(sp.SRC2[i]))

		res := src0*src1 + src2
		sp.DST[i] = uint64(math.Float32bits(res))
	}
}

func (u *ALUImpl) runVMADI32I24(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src0 := int32(bitops.SignExt(
			bitops.ExtractBitsFromU64(sp.SRC0[i], 0, 23), 23))
		src1 := int32(bitops.SignExt(
			bitops.ExtractBitsFromU64(sp.SRC1[i], 0, 23), 23))
		src2 := int32(sp.SRC2[i])

		sp.DST[i] = uint64(src0*src1 + src2)
	}
}

func (u *ALUImpl) runVMADU64U32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		sp.DST[i] = sp.SRC0[i]*sp.SRC1[i] + sp.SRC2[i]
	}
}

// runVMADI64I32 implements v_mad_i64_i32:
//   dst_i64 = sext(src0_i32) * sext(src1_i32) + src2_i64
// VOP3b's sdst (carry/overflow) is ignored — matches the existing
// runVMADU64U32 convention. Clang-ocl emits this for signed 32-bit-index
// pointer arithmetic against a 64-bit address space (struct field gather).
func (u *ALUImpl) runVMADI64I32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src0 := int64(asInt32(uint32(sp.SRC0[i])))
		src1 := int64(asInt32(uint32(sp.SRC1[i])))
		src2 := asInt64(sp.SRC2[i])
		sp.DST[i] = uint64(src0*src1 + src2)
	}
}

func (u *ALUImpl) runVMULLOU32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		sp.DST[i] = (sp.SRC0[i] * sp.SRC1[i])
	}
}

func (u *ALUImpl) runVMULHIU32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		sp.DST[i] = (sp.SRC0[i] * sp.SRC1[i]) >> 32
	}
}

// runVMULHII32 implements V_MUL_HI_I32 (VOP3a opcode 647): signed
// 32x32 multiply, returning the high 32 bits of the 64-bit product.
func (u *ALUImpl) runVMULHII32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		src0 := int64(asInt32(uint32(sp.SRC0[i])))
		src1 := int64(asInt32(uint32(sp.SRC1[i])))
		sp.DST[i] = uint64(uint32((src0 * src1) >> 32))
	}
}

func (u *ALUImpl) runVLSHLREVB64(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		sp.DST[i] = sp.SRC1[i] << sp.SRC0[i]
	}
}

// v_readlane_b32 sdst, vsrc, slane: read VGPR vsrc from lane (slane & 0x3f)
// into the scalar dst. Commit writes the SGPR once per lane, so every DST
// slot is filled with the same value (the lane value picked out of SRC0).
func (u *ALUImpl) runVREADLANEB32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	lane := sp.SRC1[0] & 0x3F
	value := sp.SRC0[lane]
	for i := 0; i < 64; i++ {
		sp.DST[i] = value
	}
}

// v_writelane_b32 vdst, ssrc, slane: write ssrc into VGPR vdst at lane index
// (slane & 0x3f). Other lanes keep their original VGPR contents — pre-populated
// in DST by the scratchpad preparer (see prepareVOP3a).
func (u *ALUImpl) runVWRITELANEB32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	lane := sp.SRC1[0] & 0x3F
	sp.DST[lane] = uint64(uint32(sp.SRC0[0]))
}

func (u *ALUImpl) runVLSHRREVB64(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		sp.DST[i] = sp.SRC1[i] >> sp.SRC0[i]
	}
}

func (u *ALUImpl) runVASHRREVI64(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		sp.DST[i] = int64ToBits(asInt64(sp.SRC1[i]) >> sp.SRC0[i])
	}
}

func (u *ALUImpl) runVADDF64(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	inst := state.Inst()
	if inst.IsSdwa == false {
		var i uint
		for i = 0; i < 64; i++ {
			if !laneMasked(sp.EXEC, i) {
				continue
			}

			src0 := math.Float64frombits(sp.SRC0[i])
			src1 := math.Float64frombits(sp.SRC1[i])
			dst := src0 + src1
			sp.DST[i] = math.Float64bits(dst)
		}
	} else {
		log.Panicf("SDWA for VOP3A instruction opcode  %d not implemented \n", inst.Opcode)
	}
}

func (u *ALUImpl) runVFMAF64(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	inst := state.Inst()
	if inst.IsSdwa == false {
		var i uint
		for i = 0; i < 64; i++ {
			if !laneMasked(sp.EXEC, i) {
				continue
			}
			src0 := math.Float64frombits(sp.SRC0[i])
			src1 := math.Float64frombits(sp.SRC1[i])
			src2 := math.Float64frombits(sp.SRC2[i])

			dst := src0*src1 + src2
			sp.DST[i] = math.Float64bits(dst)
		}
	} else {
		log.Panicf("SDWA for VOP3A instruction opcode  %d not implemented \n", inst.Opcode)
	}
}

func (u *ALUImpl) runVMIN3F32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	inst := state.Inst()
	if inst.IsSdwa == false {
		var i uint
		for i = 0; i < 64; i++ {
			if !laneMasked(sp.EXEC, i) {
				continue
			}

			src0 := math.Float32frombits(uint32(sp.SRC0[i]))
			src1 := math.Float32frombits(uint32(sp.SRC1[i]))
			src2 := math.Float32frombits(uint32(sp.SRC2[i]))

			dst := src0
			if src1 < dst {
				dst = src1
			}
			if src2 < dst {
				dst = src2
			}

			sp.DST[i] = uint64(math.Float32bits(dst))
		}
	} else {
		log.Panicf("SDWA for VOP3A instruction opcode  %d not implemented \n", inst.Opcode)
	}
}

func (u *ALUImpl) runVMIN3I32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	inst := state.Inst()
	if inst.IsSdwa == false {
		var i uint
		for i = 0; i < 64; i++ {
			if !laneMasked(sp.EXEC, i) {
				continue
			}

			src0 := asInt32(uint32(sp.SRC0[i]))
			src1 := asInt32(uint32(sp.SRC1[i]))
			src2 := asInt32(uint32(sp.SRC2[i]))

			dst := src0
			if src1 < dst {
				dst = src1
			}
			if src2 < dst {
				dst = src2
			}

			sp.DST[i] = uint64(int32ToBits(dst))
		}
	} else {
		log.Panicf("SDWA for VOP3A instruction opcode  %d not implemented \n", inst.Opcode)
	}
}

func (u *ALUImpl) runVMIN3U32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	inst := state.Inst()
	if inst.IsSdwa == false {
		var i uint
		for i = 0; i < 64; i++ {
			if !laneMasked(sp.EXEC, i) {
				continue
			}

			src0 := uint32(sp.SRC0[i])
			src1 := uint32(sp.SRC1[i])
			src2 := uint32(sp.SRC2[i])

			dst := src0
			if src1 < dst {
				dst = src1
			}
			if src2 < dst {
				dst = src2
			}

			sp.DST[i] = uint64(dst)
		}
	} else {
		log.Panicf("SDWA for VOP3A instruction opcode  %d not implemented \n", inst.Opcode)
	}
}

func (u *ALUImpl) runVMAX3F32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	inst := state.Inst()
	if inst.IsSdwa == false {
		var i uint
		for i = 0; i < 64; i++ {
			if !laneMasked(sp.EXEC, i) {
				continue
			}

			src0 := math.Float32frombits(uint32(sp.SRC0[i]))
			src1 := math.Float32frombits(uint32(sp.SRC1[i]))
			src2 := math.Float32frombits(uint32(sp.SRC2[i]))

			dst := src0
			if src1 > dst {
				dst = src1
			}
			if src2 > dst {
				dst = src2
			}

			sp.DST[i] = uint64(math.Float32bits(dst))
		}
	} else {
		log.Panicf("SDWA for VOP3A instruction opcode  %d not implemented \n", inst.Opcode)
	}
}

func (u *ALUImpl) runVMAX3I32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	inst := state.Inst()
	if inst.IsSdwa == false {
		var i uint
		for i = 0; i < 64; i++ {
			if !laneMasked(sp.EXEC, i) {
				continue
			}

			src0 := asInt32(uint32(sp.SRC0[i]))
			src1 := asInt32(uint32(sp.SRC1[i]))
			src2 := asInt32(uint32(sp.SRC2[i]))

			dst := src0
			if src1 > dst {
				dst = src1
			}
			if src2 > dst {
				dst = src2
			}

			sp.DST[i] = uint64(int32ToBits(dst))
		}
	} else {
		log.Panicf("SDWA for VOP3A instruction opcode  %d not implemented \n", inst.Opcode)
	}
}

func (u *ALUImpl) runVMAX3U32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	inst := state.Inst()
	if inst.IsSdwa == false {
		var i uint
		for i = 0; i < 64; i++ {
			if !laneMasked(sp.EXEC, i) {
				continue
			}

			src0 := uint32(sp.SRC0[i])
			src1 := uint32(sp.SRC1[i])
			src2 := uint32(sp.SRC2[i])

			dst := src0
			if src1 > dst {
				dst = src1
			}
			if src2 > dst {
				dst = src2
			}

			sp.DST[i] = uint64(dst)
		}
	} else {
		log.Panicf("SDWA for VOP3A instruction opcode  %d not implemented \n", inst.Opcode)
	}
}

func (u *ALUImpl) runVMED3F32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	inst := state.Inst()
	if inst.IsSdwa == false {
		var i uint
		for i = 0; i < 64; i++ {
			if !laneMasked(sp.EXEC, i) {
				continue
			}

			src0 := math.Float32frombits(uint32(sp.SRC0[i]))
			src1 := math.Float32frombits(uint32(sp.SRC1[i]))
			src2 := math.Float32frombits(uint32(sp.SRC2[i]))

			list := []float64{float64(src0), float64(src1), float64(src2)}
			sort.Float64s(list)

			sp.DST[i] = uint64(math.Float32bits(float32(list[1])))
		}
	} else {
		log.Panicf("SDWA for VOP3A instruction opcode  %d not implemented \n", inst.Opcode)
	}
}

func (u *ALUImpl) runVMED3I32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	inst := state.Inst()
	if inst.IsSdwa == false {
		var i uint
		for i = 0; i < 64; i++ {
			if !laneMasked(sp.EXEC, i) {
				continue
			}

			src0 := asInt32(uint32(sp.SRC0[i]))
			src1 := asInt32(uint32(sp.SRC1[i]))
			src2 := asInt32(uint32(sp.SRC2[i]))

			list := []int{int(src0), int(src1), int(src2)}
			sort.Ints(list)

			dst := int32(list[1])
			sp.DST[i] = uint64(int32ToBits(dst))
		}
	} else {
		log.Panicf("SDWA for VOP3A instruction opcode  %d not implemented \n", inst.Opcode)
	}
}

func (u *ALUImpl) runVMED3U32(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	inst := state.Inst()
	if inst.IsSdwa == false {
		var i uint
		for i = 0; i < 64; i++ {
			if !laneMasked(sp.EXEC, i) {
				continue
			}

			src0 := uint32(sp.SRC0[i])
			src1 := uint32(sp.SRC1[i])
			src2 := uint32(sp.SRC2[i])

			dst := median3Uint32(src0, src1, src2)
			sp.DST[i] = uint64(dst)
		}
	} else {
		log.Panicf("SDWA for VOP3A instruction opcode  %d not implemented \n", inst.Opcode)
	}
}

func median3Uint32(a, b, c uint32) uint32 {
	out := a

	if (b < a && b > c) || (b > a && b < c) {
		out = b
	}

	if (c < a && c > b) || (c > a && c < b) {
		out = c
	}

	return out
}

func (u *ALUImpl) runVMULF64(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	inst := state.Inst()
	if inst.IsSdwa == false {
		var i uint
		for i = 0; i < 64; i++ {
			if !laneMasked(sp.EXEC, i) {
				continue
			}
			src0 := math.Float64frombits(sp.SRC0[i])
			src1 := math.Float64frombits(sp.SRC1[i])

			dst := src0 * src1
			sp.DST[i] = math.Float64bits(dst)
		}
	} else {
		log.Panicf("SDWA for VOP3A instruction opcode  %d not implemented \n", inst.Opcode)
	}
}

func (u *ALUImpl) runVDIVFMASF64(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	inst := state.Inst()

	if inst.IsSdwa == false {
		var i uint
		for i = 0; i < 64; i++ {
			if !laneMasked(sp.EXEC, i) {
				continue
			}

			vccVal := (sp.VCC) & (1 << i)

			src0 := math.Float64frombits(sp.SRC0[i])
			src1 := math.Float64frombits(sp.SRC1[i])
			src2 := math.Float64frombits(sp.SRC2[i])

			var dst float64
			if vccVal == 1 {
				dst = math.Pow(2.0, 64) * (src0*src1 + src2)
			} else {
				dst = src0*src1 + src2
			}
			sp.DST[i] = math.Float64bits(dst)
		}
	} else {
		log.Panicf("SDWA for VOP3A instruction opcode  %d not implemented \n", inst.Opcode)
	}
}

func (u *ALUImpl) runVDIVFIXUPF64(state InstEmuState) {
	sp := state.Scratchpad().AsVOP3A()
	inst := state.Inst()

	if inst.IsSdwa {
		log.Panicf("SDWA for VOP3A instruction opcode %d not implemented \n", inst.Opcode)
	}

	var i uint
	for i = 0; i < 64; i++ {
		if !laneMasked(sp.EXEC, i) {
			continue
		}

		sp.DST[i] = u.calculateDivFixUpF64(
			sp.SRC0[i], sp.SRC1[i], sp.SRC2[i])
	}
}

//nolint:gocyclo,funlen
func (u *ALUImpl) calculateDivFixUpF64(
	src0Bits, src1Bits, src2Bits uint64,
) uint64 {
	signS1 := src1Bits >> 63
	signS2 := src2Bits >> 63
	signOut := (signS1) ^ (signS2)

	src0 := math.Float64frombits(src0Bits)
	src1 := math.Float64frombits(src1Bits)
	src2 := math.Float64frombits(src2Bits)

	exponentSrc1 := (src1Bits << 1) >> 53
	exponentSrc2 := (src2Bits << 1) >> 53

	var dst float64

	nan := math.Float64frombits(0x7FFFFFFFFFFFFFFF)
	nanWithQuieting := math.Float64frombits(0x7FF8_0000_0000_0001)
	undetermined := float64(0xFFF8_0000_0000_0000)

	if src2 == nan {
		dst = nanWithQuieting
	} else if src1 == nan {
		dst = nanWithQuieting
	} else if (src1 == 0) && (src2 == 0) {
		dst = undetermined
	} else if u.isInfByInf(src1, src2) {
		dst = undetermined
	} else if src1 == 0 || (math.Abs(src2) == 0x7FF0000000000000 || math.Abs(src2) == 0xFFF0000000000000) {
		// x/0 , or inf / y
		if signOut == 1 {
			dst = 0xFFF0000000000000 // -INF
		} else {
			dst = 0x7FF0000000000000 // +INF
		}
	} else if (math.Abs(src1) == 0x7FF0000000000000 || math.Abs(src1) == 0xFFF0000000000000) || (src2 == 0) {
		// x/inf, 0/y
		if signOut == 1 {
			dst = 0x8000000000000000 // -0
		} else {
			dst = 0x0000000000000000 // +0
		}
	} else if u.isDIVFIXUPF64Overflow(exponentSrc1, exponentSrc2) {
		log.Panicf("Underflow for VOP3A instruction DIVFIXUPF64 not implemented \n")
	} else {
		if signOut == 1 {
			dst = math.Abs(src0) * (-1.0)
		} else {
			dst = math.Abs(src0)
		}
	}

	return math.Float64bits(dst)
}

func (u *ALUImpl) isInfByInf(src1, src2 float64) bool {
	return (math.Abs(src1) == 0x7FF0000000000000 ||
		math.Abs(src1) == 0xFFF0000000000000) &&
		(math.Abs(src2) == 0x7FF0000000000000 ||
			math.Abs(src2) == 0xFFF0000000000000)
}

func (u *ALUImpl) isDIVFIXUPF64Overflow(
	exponentSrc1, exponentSrc2 uint64,
) bool {
	return int64(exponentSrc2-exponentSrc1) < -1075 ||
		exponentSrc1 == 2047
}
