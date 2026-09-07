package emu

import (
	"fmt"
	"log"
	"os"
	"strings"
)

// dumpDSAddrOOB logs full lane-level v-reg + EXEC state when a ds_read/write
// addr overflows LDS. Emits once per (wf.uid, PC) then panics so we get one
// deterministic dump per unique corruption site instead of drowning the log.
var dsOOBSeen = map[string]bool{}

func dumpDSAddrOOBAndPanic(state InstEmuState, layout *DSLayout, ldsLen int, badLane uint, badAddr uint32, op string) {
	inst := state.Inst()
	pcTag := fmt.Sprintf("%s@0x%x", op, inst.PC)
	if !dsOOBSeen[pcTag] {
		dsOOBSeen[pcTag] = true
		var b strings.Builder
		fmt.Fprintf(&b, "[ds-oob] %s inst=%s PC=0x%x exec=0x%016x ldsLen=%d badLane=%d badAddr=%d(0x%x) offset0=%d offset1=%d\n",
			op, inst.String(nil), inst.PC, layout.EXEC, ldsLen, badLane, badAddr, badAddr, inst.Offset0, inst.Offset1)
		for i := uint(0); i < 64; i++ {
			active := "-"
			if laneMasked(layout.EXEC, i) {
				active = "A"
			}
			fmt.Fprintf(&b, "  lane%02d %s ADDR=%d(0x%x)\n", i, active, layout.ADDR[i], layout.ADDR[i])
		}
		fmt.Fprint(os.Stderr, b.String())
	}
	log.Panicf("[ds-oob] %s addr=%d ldsLen=%d lane=%d exec=0x%016x — see stderr dump", op, badAddr, ldsLen, badLane, layout.EXEC)
}

func (u *ALUImpl) runDS(state InstEmuState) {
	inst := state.Inst()
	switch inst.Opcode {
	case 13:
		u.runDSWRITEB32(state)
	case 14:
		u.runDSWRITE2B32(state)
	case 54:
		u.runDSREADB32(state)
	case 55:
		u.runDSREAD2B32(state)
	case 78:
		u.runDSWRITE2B64(state)
	case 118:
		u.runDSREADB64(state)
	case 119:
		u.runDSREAD2B64(state)
	default:
		log.Panicf("Opcode %d for DS format is not implemented", inst.Opcode)
	}
}

func (u *ALUImpl) runDSWRITEB32(state InstEmuState) {
	inst := state.Inst()
	sp := state.Scratchpad()
	layout := sp.AsDS()
	lds := u.LDS()

	i := uint(0)
	for i = 0; i < 64; i++ {
		if !laneMasked(layout.EXEC, i) {
			continue
		}

		addr0 := layout.ADDR[i] + inst.Offset0
		data0offset := uint(8 + 64*4)

		copy(lds[addr0:addr0+4], sp[data0offset+i*16:data0offset+i*16+4])
	}
}

func (u *ALUImpl) runDSWRITE2B32(state InstEmuState) {
	inst := state.Inst()
	sp := state.Scratchpad()
	layout := sp.AsDS()
	lds := u.LDS()

	i := uint(0)
	for i = 0; i < 64; i++ {
		if !laneMasked(layout.EXEC, i) {
			continue
		}

		addr0 := layout.ADDR[i] + inst.Offset0*4
		data0offset := uint(8 + 64*4)
		addr1 := layout.ADDR[i] + inst.Offset1*4
		data1offset := uint(8 + 64*4 + 256*4)

		copy(lds[addr0:addr0+4], sp[data0offset+i*16:data0offset+i*16+4])
		copy(lds[addr1:addr1+4], sp[data1offset+i*16:data1offset+i*16+4])
	}
}

func (u *ALUImpl) runDSREADB32(state InstEmuState) {
	inst := state.Inst()
	sp := state.Scratchpad()
	layout := sp.AsDS()
	lds := u.LDS()

	i := uint(0)
	for i = 0; i < 64; i++ {
		if !laneMasked(layout.EXEC, i) {
			continue
		}

		addr0 := layout.ADDR[i] + inst.Offset0
		if int(addr0)+4 > len(lds) {
			dumpDSAddrOOBAndPanic(state, layout, len(lds), i, addr0, "ds_read_b32")
		}
		// addr0 := layout.ADDR[i]
		dstOffset := uint(8 + 64*4 + 256*4*2)
		copy(sp[dstOffset+i*16:dstOffset+i*16+4], lds[addr0:addr0+4])
	}
}

func (u *ALUImpl) runDSREAD2B32(state InstEmuState) {
	inst := state.Inst()
	sp := state.Scratchpad()
	layout := sp.AsDS()
	lds := u.LDS()

	i := uint(0)
	for i = 0; i < 64; i++ {
		if !laneMasked(layout.EXEC, i) {
			continue
		}

		addr0 := layout.ADDR[i] + inst.Offset0*4
		dstOffset := uint(8 + 64*4 + 256*4*2)
		copy(sp[dstOffset+i*16:dstOffset+i*16+4], lds[addr0:addr0+4])

		addr1 := layout.ADDR[i] + inst.Offset1*4
		copy(sp[dstOffset+i*16+4:dstOffset+i*16+8], lds[addr1:addr1+4])
	}
}

func (u *ALUImpl) runDSWRITE2B64(state InstEmuState) {
	inst := state.Inst()
	sp := state.Scratchpad()
	layout := sp.AsDS()
	lds := u.LDS()

	i := uint(0)
	for i = 0; i < 64; i++ {
		if !laneMasked(layout.EXEC, i) {
			continue
		}

		addr0 := layout.ADDR[i] + inst.Offset0*8
		data0Offset := uint(8 + 64*4)
		copy(lds[addr0:addr0+8], sp[data0Offset+i*16:data0Offset+i*16+8])

		addr1 := layout.ADDR[i] + inst.Offset1*8
		data1Offset := uint(8 + 64*4 + 256*4)
		copy(lds[addr1:addr1+8], sp[data1Offset+i*16:data1Offset+i*16+8])
	}
}

func (u *ALUImpl) runDSREADB64(state InstEmuState) {
	sp := state.Scratchpad()
	layout := sp.AsDS()
	lds := u.LDS()

	i := uint(0)
	for i = 0; i < 64; i++ {
		if !laneMasked(layout.EXEC, i) {
			continue
		}

		addr := layout.ADDR[i]
		dstOffset := uint(8 + 64*4 + 256*4*2)
		copy(sp[dstOffset+i*16:dstOffset+i*16+8], lds[addr:addr+8])
	}
}

func (u *ALUImpl) runDSREAD2B64(state InstEmuState) {
	inst := state.Inst()
	sp := state.Scratchpad()
	layout := sp.AsDS()
	lds := u.LDS()

	i := uint(0)
	for i = 0; i < 64; i++ {
		if !laneMasked(layout.EXEC, i) {
			continue
		}

		addr0 := layout.ADDR[i] + inst.Offset0*8
		dstOffset := uint(8 + 64*4 + 256*4*2)
		copy(sp[dstOffset+i*16:dstOffset+i*16+8], lds[addr0:addr0+8])

		addr1 := layout.ADDR[i] + inst.Offset1*8
		copy(sp[dstOffset+i*16+8:dstOffset+i*16+16], lds[addr1:addr1+8])
	}
}
