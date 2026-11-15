//go:build arm64

#include "textflag.h"

// Dot products of stored int8 code rows with the query codes.
//
// Stored code bytes are c+128; flipping the top bit gives the signed code,
// so every kernel returns sum_d (u[d]-128)*e[d] exactly. dim is a multiple
// of 16. The integer multiplies are not in the Go assembler's NEON subset,
// so they are emitted as words with the A64 mnemonic beside each (checked
// with go tool objdump); q8kernel_test.go compares all kernels with the Go
// reference on arm64 hardware in CI.
//
// dotS8*:  base NEON. SMULL/SMULL2 give int16 products (|p| <= 128*63) and
//          SADALP adds pairs of them into int32 lanes.
// dotSD*:  ARMv8.2 dot product (SDOT), used when the CPU reports asimddp.

// func dotS8Block(c *int8, e *int8, dim int, rows int, out *int32)
TEXT ·dotS8Block(SB), NOSPLIT, $0-40
	MOVD c+0(FP), R0
	MOVD e+8(FP), R1
	MOVD dim+16(FP), R2
	MOVD rows+24(FP), R3
	MOVD out+32(FP), R4
	VMOVI $128, V31.B16
	CBZ R3, adone
arow:
	MOVD R1, R5
	MOVD R2, R6
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16
	VEOR V16.B16, V16.B16, V16.B16
	VEOR V17.B16, V17.B16, V17.B16
	CMP $32, R6
	BLT atail
aloop32:
	VLD1.P 32(R0), [V2.B16, V3.B16]
	VLD1.P 32(R5), [V4.B16, V5.B16]
	VEOR V31.B16, V2.B16, V2.B16
	VEOR V31.B16, V3.B16, V3.B16
	WORD $0x0E24C046 // smull  v6.8h, v2.8b, v4.8b
	WORD $0x4E24C047 // smull2 v7.8h, v2.16b, v4.16b
	WORD $0x0E25C072 // smull  v18.8h, v3.8b, v5.8b
	WORD $0x4E25C073 // smull2 v19.8h, v3.16b, v5.16b
	WORD $0x4E6068C0 // sadalp v0.4s, v6.8h
	WORD $0x4E6068E1 // sadalp v1.4s, v7.8h
	WORD $0x4E606A50 // sadalp v16.4s, v18.8h
	WORD $0x4E606A71 // sadalp v17.4s, v19.8h
	SUB $32, R6
	CMP $32, R6
	BGE aloop32
atail:
	CBZ R6, asum
	VLD1.P 16(R0), [V2.B16]
	VLD1.P 16(R5), [V4.B16]
	VEOR V31.B16, V2.B16, V2.B16
	WORD $0x0E24C046 // smull  v6.8h, v2.8b, v4.8b
	WORD $0x4E24C047 // smull2 v7.8h, v2.16b, v4.16b
	WORD $0x4E6068C0 // sadalp v0.4s, v6.8h
	WORD $0x4E6068E1 // sadalp v1.4s, v7.8h
asum:
	VADD V1.S4, V0.S4, V0.S4
	VADD V17.S4, V16.S4, V16.S4
	VADD V16.S4, V0.S4, V0.S4
	VADDV V0.S4, V0
	VMOV V0.S[0], R7
	MOVW R7, (R4)
	ADD $4, R4
	SUBS $1, R3
	BNE arow
adone:
	RET

// func dotS8Rows(c *int8, idx *int32, n int, e *int8, dim int, out *int32)
TEXT ·dotS8Rows(SB), NOSPLIT, $0-48
	MOVD c+0(FP), R8
	MOVD idx+8(FP), R9
	MOVD n+16(FP), R3
	MOVD e+24(FP), R1
	MOVD dim+32(FP), R2
	MOVD out+40(FP), R4
	VMOVI $128, V31.B16
	CBZ R3, bdone
brow:
	MOVW.P 4(R9), R10
	MUL R2, R10, R10
	ADD R8, R10, R0
	MOVD R1, R5
	MOVD R2, R6
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16
	VEOR V16.B16, V16.B16, V16.B16
	VEOR V17.B16, V17.B16, V17.B16
	CMP $32, R6
	BLT btail
bloop32:
	VLD1.P 32(R0), [V2.B16, V3.B16]
	VLD1.P 32(R5), [V4.B16, V5.B16]
	VEOR V31.B16, V2.B16, V2.B16
	VEOR V31.B16, V3.B16, V3.B16
	WORD $0x0E24C046 // smull  v6.8h, v2.8b, v4.8b
	WORD $0x4E24C047 // smull2 v7.8h, v2.16b, v4.16b
	WORD $0x0E25C072 // smull  v18.8h, v3.8b, v5.8b
	WORD $0x4E25C073 // smull2 v19.8h, v3.16b, v5.16b
	WORD $0x4E6068C0 // sadalp v0.4s, v6.8h
	WORD $0x4E6068E1 // sadalp v1.4s, v7.8h
	WORD $0x4E606A50 // sadalp v16.4s, v18.8h
	WORD $0x4E606A71 // sadalp v17.4s, v19.8h
	SUB $32, R6
	CMP $32, R6
	BGE bloop32
btail:
	CBZ R6, bsum
	VLD1.P 16(R0), [V2.B16]
	VLD1.P 16(R5), [V4.B16]
	VEOR V31.B16, V2.B16, V2.B16
	WORD $0x0E24C046 // smull  v6.8h, v2.8b, v4.8b
	WORD $0x4E24C047 // smull2 v7.8h, v2.16b, v4.16b
	WORD $0x4E6068C0 // sadalp v0.4s, v6.8h
	WORD $0x4E6068E1 // sadalp v1.4s, v7.8h
bsum:
	VADD V1.S4, V0.S4, V0.S4
	VADD V17.S4, V16.S4, V16.S4
	VADD V16.S4, V0.S4, V0.S4
	VADDV V0.S4, V0
	VMOV V0.S[0], R7
	MOVW R7, (R4)
	ADD $4, R4
	SUBS $1, R3
	BNE brow
bdone:
	RET

// func dotSDBlock(c *int8, e *int8, dim int, rows int, out *int32)
TEXT ·dotSDBlock(SB), NOSPLIT, $0-40
	MOVD c+0(FP), R0
	MOVD e+8(FP), R1
	MOVD dim+16(FP), R2
	MOVD rows+24(FP), R3
	MOVD out+32(FP), R4
	VMOVI $128, V31.B16
	CBZ R3, cdone
crow:
	MOVD R1, R5
	MOVD R2, R6
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16
	VEOR V16.B16, V16.B16, V16.B16
	VEOR V17.B16, V17.B16, V17.B16
	CMP $64, R6
	BLT ctail
cloop64:
	VLD1.P 64(R0), [V2.B16, V3.B16, V4.B16, V5.B16]
	VLD1.P 64(R5), [V20.B16, V21.B16, V22.B16, V23.B16]
	VEOR V31.B16, V2.B16, V2.B16
	VEOR V31.B16, V3.B16, V3.B16
	VEOR V31.B16, V4.B16, V4.B16
	VEOR V31.B16, V5.B16, V5.B16
	WORD $0x4E949440 // sdot v0.4s, v2.16b, v20.16b
	WORD $0x4E959461 // sdot v1.4s, v3.16b, v21.16b
	WORD $0x4E969490 // sdot v16.4s, v4.16b, v22.16b
	WORD $0x4E9794B1 // sdot v17.4s, v5.16b, v23.16b
	SUB $64, R6
	CMP $64, R6
	BGE cloop64
ctail:
	CBZ R6, csum
cloop16:
	VLD1.P 16(R0), [V2.B16]
	VLD1.P 16(R5), [V20.B16]
	VEOR V31.B16, V2.B16, V2.B16
	WORD $0x4E949440 // sdot v0.4s, v2.16b, v20.16b
	SUBS $16, R6
	BNE cloop16
csum:
	VADD V1.S4, V0.S4, V0.S4
	VADD V17.S4, V16.S4, V16.S4
	VADD V16.S4, V0.S4, V0.S4
	VADDV V0.S4, V0
	VMOV V0.S[0], R7
	MOVW R7, (R4)
	ADD $4, R4
	SUBS $1, R3
	BNE crow
cdone:
	RET

// func dotSDRows(c *int8, idx *int32, n int, e *int8, dim int, out *int32)
TEXT ·dotSDRows(SB), NOSPLIT, $0-48
	MOVD c+0(FP), R8
	MOVD idx+8(FP), R9
	MOVD n+16(FP), R3
	MOVD e+24(FP), R1
	MOVD dim+32(FP), R2
	MOVD out+40(FP), R4
	VMOVI $128, V31.B16
	CBZ R3, ddone
drow:
	MOVW.P 4(R9), R10
	MUL R2, R10, R10
	ADD R8, R10, R0
	MOVD R1, R5
	MOVD R2, R6
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16
	VEOR V16.B16, V16.B16, V16.B16
	VEOR V17.B16, V17.B16, V17.B16
	CMP $64, R6
	BLT dtail
dloop64:
	VLD1.P 64(R0), [V2.B16, V3.B16, V4.B16, V5.B16]
	VLD1.P 64(R5), [V20.B16, V21.B16, V22.B16, V23.B16]
	VEOR V31.B16, V2.B16, V2.B16
	VEOR V31.B16, V3.B16, V3.B16
	VEOR V31.B16, V4.B16, V4.B16
	VEOR V31.B16, V5.B16, V5.B16
	WORD $0x4E949440 // sdot v0.4s, v2.16b, v20.16b
	WORD $0x4E959461 // sdot v1.4s, v3.16b, v21.16b
	WORD $0x4E969490 // sdot v16.4s, v4.16b, v22.16b
	WORD $0x4E9794B1 // sdot v17.4s, v5.16b, v23.16b
	SUB $64, R6
	CMP $64, R6
	BGE dloop64
dtail:
	CBZ R6, dsum
dloop16:
	VLD1.P 16(R0), [V2.B16]
	VLD1.P 16(R5), [V20.B16]
	VEOR V31.B16, V2.B16, V2.B16
	WORD $0x4E949440 // sdot v0.4s, v2.16b, v20.16b
	SUBS $16, R6
	BNE dloop16
dsum:
	VADD V1.S4, V0.S4, V0.S4
	VADD V17.S4, V16.S4, V16.S4
	VADD V16.S4, V0.S4, V0.S4
	VADDV V0.S4, V0
	VMOV V0.S[0], R7
	MOVW R7, (R4)
	ADD $4, R4
	SUBS $1, R3
	BNE drow
ddone:
	RET
