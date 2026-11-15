//go:build arm64

#include "textflag.h"

// The integer multiplies are not in the Go assembler's NEON subset, so they
// are emitted as words; each has its A64 mnemonic beside it and the build
// test disassembles them (see q8_arm64_test.go for the result check).
//
// Stored code bytes are c+128; flipping the top bit gives the signed code,
// so each kernel returns sum_d (u[d]-128)*e[d] exactly: the products fit in
// int16 (128*63) and pairs of them are accumulated into int32 lanes.

// func dotS8Block(c *int8, e *int8, dim int, rows int, out *int32)
// out[r] = dot of code row r with e, for r < rows; dim is a multiple of 16.
TEXT ·dotS8Block(SB), NOSPLIT, $0-40
	MOVD c+0(FP), R0
	MOVD e+8(FP), R1
	MOVD dim+16(FP), R2
	MOVD rows+24(FP), R3
	MOVD out+32(FP), R4
	VMOVI $128, V31.B16
	CBZ R3, bdone
brow:
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

// func dotS8Rows(c *int8, idx *int32, n int, e *int8, dim int, out *int32)
// out[i] = dot of code row idx[i] with e, for i < n.
TEXT ·dotS8Rows(SB), NOSPLIT, $0-48
	MOVD c+0(FP), R8
	MOVD idx+8(FP), R9
	MOVD n+16(FP), R3
	MOVD e+24(FP), R1
	MOVD dim+32(FP), R2
	MOVD out+40(FP), R4
	VMOVI $128, V31.B16
	CBZ R3, rdone
rrow:
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
	BLT rtail
rloop32:
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
	BGE rloop32
rtail:
	CBZ R6, rsum
	VLD1.P 16(R0), [V2.B16]
	VLD1.P 16(R5), [V4.B16]
	VEOR V31.B16, V2.B16, V2.B16
	WORD $0x0E24C046 // smull  v6.8h, v2.8b, v4.8b
	WORD $0x4E24C047 // smull2 v7.8h, v2.16b, v4.16b
	WORD $0x4E6068C0 // sadalp v0.4s, v6.8h
	WORD $0x4E6068E1 // sadalp v1.4s, v7.8h
rsum:
	VADD V1.S4, V0.S4, V0.S4
	VADD V17.S4, V16.S4, V16.S4
	VADD V16.S4, V0.S4, V0.S4
	VADDV V0.S4, V0
	VMOV V0.S[0], R7
	MOVW R7, (R4)
	ADD $4, R4
	SUBS $1, R3
	BNE rrow
rdone:
	RET
