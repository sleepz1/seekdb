//go:build amd64

#include "textflag.h"

// func l2sqAVX(a, b []float32) float32
TEXT ·l2sqAVX(SB), NOSPLIT, $0-52
	MOVQ a_base+0(FP), SI
	MOVQ a_len+8(FP), CX
	MOVQ b_base+24(FP), DI
	VXORPS Y0, Y0, Y0
	VXORPS Y1, Y1, Y1
	VXORPS Y2, Y2, Y2
	VXORPS Y3, Y3, Y3

loop32:
	CMPQ CX, $32
	JL   loop8
	VMOVUPS (SI), Y4
	VMOVUPS 32(SI), Y5
	VMOVUPS 64(SI), Y6
	VMOVUPS 96(SI), Y7
	VSUBPS  (DI), Y4, Y4
	VSUBPS  32(DI), Y5, Y5
	VSUBPS  64(DI), Y6, Y6
	VSUBPS  96(DI), Y7, Y7
	VMULPS  Y4, Y4, Y4
	VMULPS  Y5, Y5, Y5
	VMULPS  Y6, Y6, Y6
	VMULPS  Y7, Y7, Y7
	VADDPS  Y4, Y0, Y0
	VADDPS  Y5, Y1, Y1
	VADDPS  Y6, Y2, Y2
	VADDPS  Y7, Y3, Y3
	ADDQ    $128, SI
	ADDQ    $128, DI
	SUBQ    $32, CX
	JMP     loop32

loop8:
	CMPQ CX, $8
	JL   reduce
	VMOVUPS (SI), Y4
	VSUBPS  (DI), Y4, Y4
	VMULPS  Y4, Y4, Y4
	VADDPS  Y4, Y0, Y0
	ADDQ    $32, SI
	ADDQ    $32, DI
	SUBQ    $8, CX
	JMP     loop8

reduce:
	VADDPS       Y1, Y0, Y0
	VADDPS       Y3, Y2, Y2
	VADDPS       Y2, Y0, Y0
	VEXTRACTF128 $1, Y0, X1
	VADDPS       X1, X0, X0
	VHADDPS      X0, X0, X0
	VHADDPS      X0, X0, X0

tail:
	CMPQ   CX, $0
	JE     done
	VMOVSS (SI), X4
	VSUBSS (DI), X4, X4
	VMULSS X4, X4, X4
	VADDSS X4, X0, X0
	ADDQ   $4, SI
	ADDQ   $4, DI
	DECQ   CX
	JMP    tail

done:
	VZEROUPPER
	MOVSS X0, ret+48(FP)
	RET

// func dotU8Block(c []int8, e []int8, bias int32, out []int32)
// c holds rows of len(e) codes stored with the sign bit flipped (unsigned
// u = c+128); e is the query, |e| <= 63, so the pair sums of VPMADDUBSW
// (<= 2*255*63 = 32130) cannot saturate. out[r] = sum u*e - bias.
// Four rows per step, one horizontal reduction for the four.
TEXT ·dotU8Block(SB), NOSPLIT, $0-80
	MOVQ         c_base+0(FP), SI
	MOVQ         e_base+24(FP), R8
	MOVQ         e_len+32(FP), R9
	MOVL         bias+48(FP), AX
	MOVQ         out_base+56(FP), R10
	MOVQ         out_len+64(FP), R11
	VPCMPEQW     Y15, Y15, Y15
	VPSRLW       $15, Y15, Y15
	VMOVD        AX, X14
	VPBROADCASTD X14, X14
	MOVQ         R9, CX
	ANDQ         $-32, CX

b4:
	CMPQ  R11, $4
	JL    b1
	LEAQ  (SI)(R9*1), R12
	LEAQ  (R12)(R9*1), R13
	LEAQ  (R13)(R9*1), BX
	VPXOR Y0, Y0, Y0
	VPXOR Y1, Y1, Y1
	VPXOR Y2, Y2, Y2
	VPXOR Y3, Y3, Y3
	XORQ  DX, DX

b4loop:
	CMPQ       DX, CX
	JGE        b4tail
	VMOVDQU    (R8)(DX*1), Y8
	VMOVDQU    (SI)(DX*1), Y4
	VMOVDQU    (R12)(DX*1), Y5
	VMOVDQU    (R13)(DX*1), Y6
	VMOVDQU    (BX)(DX*1), Y7
	VPMADDUBSW Y8, Y4, Y4
	VPMADDUBSW Y8, Y5, Y5
	VPMADDUBSW Y8, Y6, Y6
	VPMADDUBSW Y8, Y7, Y7
	VPMADDWD   Y15, Y4, Y4
	VPMADDWD   Y15, Y5, Y5
	VPMADDWD   Y15, Y6, Y6
	VPMADDWD   Y15, Y7, Y7
	VPADDD     Y4, Y0, Y0
	VPADDD     Y5, Y1, Y1
	VPADDD     Y6, Y2, Y2
	VPADDD     Y7, Y3, Y3
	ADDQ       $32, DX
	JMP        b4loop

b4tail:
	CMPQ       DX, R9
	JGE        b4red
	VMOVDQU    (R8)(DX*1), X8
	VMOVDQU    (SI)(DX*1), X4
	VMOVDQU    (R12)(DX*1), X5
	VMOVDQU    (R13)(DX*1), X6
	VMOVDQU    (BX)(DX*1), X7
	VPMADDUBSW X8, X4, X4
	VPMADDUBSW X8, X5, X5
	VPMADDUBSW X8, X6, X6
	VPMADDUBSW X8, X7, X7
	VPMADDWD   X15, X4, X4
	VPMADDWD   X15, X5, X5
	VPMADDWD   X15, X6, X6
	VPMADDWD   X15, X7, X7
	VPADDD     Y4, Y0, Y0
	VPADDD     Y5, Y1, Y1
	VPADDD     Y6, Y2, Y2
	VPADDD     Y7, Y3, Y3

b4red:
	VPHADDD      Y1, Y0, Y0
	VPHADDD      Y3, Y2, Y2
	VPHADDD      Y2, Y0, Y0
	VEXTRACTI128 $1, Y0, X1
	VPADDD       X1, X0, X0
	VPSUBD       X14, X0, X0
	VMOVDQU      X0, (R10)
	ADDQ         $16, R10
	LEAQ         (BX)(R9*1), SI
	SUBQ         $4, R11
	JMP          b4

b1:
	CMPQ  R11, $0
	JE    bdone
	VPXOR Y0, Y0, Y0
	XORQ  DX, DX

b1loop:
	CMPQ       DX, CX
	JGE        b1tail
	VMOVDQU    (SI)(DX*1), Y4
	VPMADDUBSW (R8)(DX*1), Y4, Y4
	VPMADDWD   Y15, Y4, Y4
	VPADDD     Y4, Y0, Y0
	ADDQ       $32, DX
	JMP        b1loop

b1tail:
	CMPQ       DX, R9
	JGE        b1red
	VMOVDQU    (SI)(DX*1), X4
	VPMADDUBSW (R8)(DX*1), X4, X4
	VPMADDWD   X15, X4, X4
	VPADDD     Y4, Y0, Y0

b1red:
	VEXTRACTI128 $1, Y0, X1
	VPADDD       X1, X0, X0
	VPHADDD      X0, X0, X0
	VPHADDD      X0, X0, X0
	VPSUBD       X14, X0, X0
	VMOVD        X0, (R10)
	ADDQ         $4, R10
	ADDQ         R9, SI
	DECQ         R11
	JMP          b1

bdone:
	VZEROUPPER
	RET

// func dotU8Rows(c []int8, idx []int32, e []int8, bias int32, out []int32)
// out[i] = the dotU8Block value of row idx[i] of c.
TEXT ·dotU8Rows(SB), NOSPLIT, $0-104
	MOVQ         c_base+0(FP), R12
	MOVQ         idx_base+24(FP), R13
	MOVQ         idx_len+32(FP), R11
	MOVQ         e_base+48(FP), R8
	MOVQ         e_len+56(FP), R9
	MOVL         bias+72(FP), AX
	MOVQ         out_base+80(FP), R10
	VPCMPEQW     Y15, Y15, Y15
	VPSRLW       $15, Y15, Y15
	VMOVD        AX, X14
	VPBROADCASTD X14, X14
	MOVQ         R9, CX
	ANDQ         $-32, CX

rrow:
	CMPQ    R11, $0
	JE      rdone
	MOVLQSX (R13), SI
	IMULQ   R9, SI
	ADDQ    R12, SI
	VPXOR   Y0, Y0, Y0
	XORQ    DX, DX

rloop:
	CMPQ       DX, CX
	JGE        rtail
	VMOVDQU    (SI)(DX*1), Y4
	VPMADDUBSW (R8)(DX*1), Y4, Y4
	VPMADDWD   Y15, Y4, Y4
	VPADDD     Y4, Y0, Y0
	ADDQ       $32, DX
	JMP        rloop

rtail:
	CMPQ       DX, R9
	JGE        rred
	VMOVDQU    (SI)(DX*1), X4
	VPMADDUBSW (R8)(DX*1), X4, X4
	VPMADDWD   X15, X4, X4
	VPADDD     Y4, Y0, Y0

rred:
	VEXTRACTI128 $1, Y0, X1
	VPADDD       X1, X0, X0
	VPHADDD      X0, X0, X0
	VPHADDD      X0, X0, X0
	VPSUBD       X14, X0, X0
	VMOVD        X0, (R10)
	ADDQ         $4, R10
	ADDQ         $4, R13
	DECQ         R11
	JMP          rrow

rdone:
	VZEROUPPER
	RET

// func dotU8BlockVNNI(c []int8, e []int8, bias int32, out []int32)
// dotU8Block with VPDPBUSD (AVX512-VNNI, 256-bit form, needs AVX512VL):
// one instruction per 32 bytes instead of VPMADDUBSW+VPMADDWD+VPADDD. The
// sums are exact in both (no saturation, see dotU8Block), so the results
// are identical; the 16-byte tails use the AVX2 sequence.
TEXT ·dotU8BlockVNNI(SB), NOSPLIT, $0-80
	MOVQ         c_base+0(FP), SI
	MOVQ         e_base+24(FP), R8
	MOVQ         e_len+32(FP), R9
	MOVL         bias+48(FP), AX
	MOVQ         out_base+56(FP), R10
	MOVQ         out_len+64(FP), R11
	VPCMPEQW     Y15, Y15, Y15
	VPSRLW       $15, Y15, Y15
	VMOVD        AX, X14
	VPBROADCASTD X14, X14
	MOVQ         R9, CX
	ANDQ         $-32, CX

b4:
	CMPQ  R11, $4
	JL    b1
	LEAQ  (SI)(R9*1), R12
	LEAQ  (R12)(R9*1), R13
	LEAQ  (R13)(R9*1), BX
	VPXOR Y0, Y0, Y0
	VPXOR Y1, Y1, Y1
	VPXOR Y2, Y2, Y2
	VPXOR Y3, Y3, Y3
	XORQ  DX, DX

b4loop:
	CMPQ       DX, CX
	JGE        b4tail
	VMOVDQU    (R8)(DX*1), Y8
	VMOVDQU    (SI)(DX*1), Y4
	VMOVDQU    (R12)(DX*1), Y5
	VMOVDQU    (R13)(DX*1), Y6
	VMOVDQU    (BX)(DX*1), Y7
	VPDPBUSD   Y8, Y4, Y0
	VPDPBUSD   Y8, Y5, Y1
	VPDPBUSD   Y8, Y6, Y2
	VPDPBUSD   Y8, Y7, Y3
	ADDQ       $32, DX
	JMP        b4loop

b4tail:
	CMPQ       DX, R9
	JGE        b4red
	VMOVDQU    (R8)(DX*1), X8
	VMOVDQU    (SI)(DX*1), X4
	VMOVDQU    (R12)(DX*1), X5
	VMOVDQU    (R13)(DX*1), X6
	VMOVDQU    (BX)(DX*1), X7
	VPMADDUBSW X8, X4, X4
	VPMADDUBSW X8, X5, X5
	VPMADDUBSW X8, X6, X6
	VPMADDUBSW X8, X7, X7
	VPMADDWD   X15, X4, X4
	VPMADDWD   X15, X5, X5
	VPMADDWD   X15, X6, X6
	VPMADDWD   X15, X7, X7
	VPADDD     Y4, Y0, Y0
	VPADDD     Y5, Y1, Y1
	VPADDD     Y6, Y2, Y2
	VPADDD     Y7, Y3, Y3

b4red:
	VPHADDD      Y1, Y0, Y0
	VPHADDD      Y3, Y2, Y2
	VPHADDD      Y2, Y0, Y0
	VEXTRACTI128 $1, Y0, X1
	VPADDD       X1, X0, X0
	VPSUBD       X14, X0, X0
	VMOVDQU      X0, (R10)
	ADDQ         $16, R10
	LEAQ         (BX)(R9*1), SI
	SUBQ         $4, R11
	JMP          b4

b1:
	CMPQ  R11, $0
	JE    bdone
	VPXOR Y0, Y0, Y0
	XORQ  DX, DX

b1loop:
	CMPQ       DX, CX
	JGE        b1tail
	VMOVDQU    (SI)(DX*1), Y4
	VPDPBUSD   (R8)(DX*1), Y4, Y0
	ADDQ       $32, DX
	JMP        b1loop

b1tail:
	CMPQ       DX, R9
	JGE        b1red
	VMOVDQU    (SI)(DX*1), X4
	VPMADDUBSW (R8)(DX*1), X4, X4
	VPMADDWD   X15, X4, X4
	VPADDD     Y4, Y0, Y0

b1red:
	VEXTRACTI128 $1, Y0, X1
	VPADDD       X1, X0, X0
	VPHADDD      X0, X0, X0
	VPHADDD      X0, X0, X0
	VPSUBD       X14, X0, X0
	VMOVD        X0, (R10)
	ADDQ         $4, R10
	ADDQ         R9, SI
	DECQ         R11
	JMP          b1

bdone:
	VZEROUPPER
	RET

// func dotU8RowsVNNI(c []int8, idx []int32, e []int8, bias int32, out []int32)
// out[i] = the dotU8Block value of row idx[i] of c.
TEXT ·dotU8RowsVNNI(SB), NOSPLIT, $0-104
	MOVQ         c_base+0(FP), R12
	MOVQ         idx_base+24(FP), R13
	MOVQ         idx_len+32(FP), R11
	MOVQ         e_base+48(FP), R8
	MOVQ         e_len+56(FP), R9
	MOVL         bias+72(FP), AX
	MOVQ         out_base+80(FP), R10
	VPCMPEQW     Y15, Y15, Y15
	VPSRLW       $15, Y15, Y15
	VMOVD        AX, X14
	VPBROADCASTD X14, X14
	MOVQ         R9, CX
	ANDQ         $-32, CX

rrow:
	CMPQ    R11, $0
	JE      rdone
	MOVLQSX (R13), SI
	IMULQ   R9, SI
	ADDQ    R12, SI
	VPXOR   Y0, Y0, Y0
	XORQ    DX, DX

rloop:
	CMPQ       DX, CX
	JGE        rtail
	VMOVDQU    (SI)(DX*1), Y4
	VPDPBUSD   (R8)(DX*1), Y4, Y0
	ADDQ       $32, DX
	JMP        rloop

rtail:
	CMPQ       DX, R9
	JGE        rred
	VMOVDQU    (SI)(DX*1), X4
	VPMADDUBSW (R8)(DX*1), X4, X4
	VPMADDWD   X15, X4, X4
	VPADDD     Y4, Y0, Y0

rred:
	VEXTRACTI128 $1, Y0, X1
	VPADDD       X1, X0, X0
	VPHADDD      X0, X0, X0
	VPHADDD      X0, X0, X0
	VPSUBD       X14, X0, X0
	VMOVD        X0, (R10)
	ADDQ         $4, R10
	ADDQ         $4, R13
	DECQ         R11
	JMP          rrow

rdone:
	VZEROUPPER
	RET


// func pruneMask(s []float32, c2 []int32, r []float32, d []int32, k *pruneK, out []byte)
// Per row (float64, same operation order as q8query.prune):
//   a = (s*s)*c2; b = ((s+s)*t)*d; val = ((a-b)+c) - ((a+|b|)+c)*K
//   thr = (kth+r)+rho; pruned = val > 0 && val > thr*thr
TEXT ·pruneMask(SB), NOSPLIT, $0-128
	MOVQ s_base+0(FP), SI
	MOVQ c2_base+24(FP), DI
	MOVQ r_base+48(FP), R8
	MOVQ d_base+72(FP), R9
	MOVQ d_len+80(FP), CX
	MOVQ k+96(FP), R10
	MOVQ out_base+104(FP), R11
	VBROADCASTSD 0(R10), Y10
	VBROADCASTSD 8(R10), Y11
	VBROADCASTSD 16(R10), Y12
	VBROADCASTSD 24(R10), Y13
	VBROADCASTSD 32(R10), Y14
	VPCMPEQD     Y15, Y15, Y15
	VPSRLQ       $1, Y15, Y15
	VXORPD       Y9, Y9, Y9

ploop:
	CMPQ      CX, $4
	JL        pdone
	VCVTPS2PD (SI), Y0
	VCVTDQ2PD (DI), Y1
	VCVTDQ2PD (R9), Y2
	VCVTPS2PD (R8), Y3
	VMULPD    Y0, Y0, Y4
	VMULPD    Y1, Y4, Y4
	VADDPD    Y0, Y0, Y5
	VMULPD    Y10, Y5, Y5
	VMULPD    Y2, Y5, Y5
	VSUBPD    Y5, Y4, Y6
	VADDPD    Y11, Y6, Y6
	VANDPD    Y15, Y5, Y7
	VADDPD    Y7, Y4, Y7
	VADDPD    Y11, Y7, Y7
	VMULPD    Y12, Y7, Y7
	VSUBPD    Y7, Y6, Y6
	VADDPD    Y3, Y13, Y8
	VADDPD    Y14, Y8, Y8
	VMULPD    Y8, Y8, Y8
	VCMPPD    $0x1E, Y9, Y6, Y0
	VCMPPD    $0x1E, Y8, Y6, Y1
	VANDPD    Y1, Y0, Y0
	VMOVMSKPD Y0, AX
	MOVB      AL, (R11)
	ADDQ      $16, SI
	ADDQ      $16, DI
	ADDQ      $16, R8
	ADDQ      $16, R9
	INCQ      R11
	SUBQ      $4, CX
	JMP       ploop

pdone:
	VZEROUPPER
	RET

// func cpuid(eaxArg, ecxArg uint32) (eax, ebx, ecx, edx uint32)
TEXT ·cpuid(SB), NOSPLIT, $0-24
	MOVL eaxArg+0(FP), AX
	MOVL ecxArg+4(FP), CX
	CPUID
	MOVL AX, eax+8(FP)
	MOVL BX, ebx+12(FP)
	MOVL CX, ecx+16(FP)
	MOVL DX, edx+20(FP)
	RET

// func xgetbv() (eax, edx uint32)
TEXT ·xgetbv(SB), NOSPLIT, $0-8
	MOVL $0, CX
	XGETBV
	MOVL AX, eax+0(FP)
	MOVL DX, edx+4(FP)
	RET

// func dotU8Block128x8VNNI(c []int8, e []int8, bias int32, out []int32)
// dotU8Block for 128-dimensional rows, eight rows per step: the query stays
// in Y8-Y11, eight independent accumulators hide the VPDPBUSD latency, and
// one reduction yields the eight sums. len(out) must be a multiple of 8.
TEXT ·dotU8Block128x8VNNI(SB), NOSPLIT, $0-80
	MOVQ         c_base+0(FP), SI
	MOVQ         e_base+24(FP), R8
	MOVL         bias+48(FP), AX
	MOVQ         out_base+56(FP), R10
	MOVQ         out_len+64(FP), R11
	VPCMPEQW     Y15, Y15, Y15
	VPSRLW       $15, Y15, Y15
	VMOVD        AX, X14
	VPBROADCASTD X14, Y14
	VMOVDQU      0(R8), Y8
	VMOVDQU      32(R8), Y9
	VMOVDQU      64(R8), Y10
	VMOVDQU      96(R8), Y11

dotU8Block128x8VNNI_loop:
	CMPQ  R11, $8
	JL    dotU8Block128x8VNNI_done
	VPXOR Y0, Y0, Y0
	VPXOR Y1, Y1, Y1
	VPXOR Y2, Y2, Y2
	VPXOR Y3, Y3, Y3
	VPXOR Y4, Y4, Y4
	VPXOR Y5, Y5, Y5
	VPXOR Y6, Y6, Y6
	VPXOR Y7, Y7, Y7
	VMOVDQU  0(SI), Y12
	VPDPBUSD Y8, Y12, Y0
	VMOVDQU  128(SI), Y13
	VPDPBUSD Y8, Y13, Y1
	VMOVDQU  256(SI), Y12
	VPDPBUSD Y8, Y12, Y2
	VMOVDQU  384(SI), Y13
	VPDPBUSD Y8, Y13, Y3
	VMOVDQU  512(SI), Y12
	VPDPBUSD Y8, Y12, Y4
	VMOVDQU  640(SI), Y13
	VPDPBUSD Y8, Y13, Y5
	VMOVDQU  768(SI), Y12
	VPDPBUSD Y8, Y12, Y6
	VMOVDQU  896(SI), Y13
	VPDPBUSD Y8, Y13, Y7
	VMOVDQU  32(SI), Y12
	VPDPBUSD Y9, Y12, Y0
	VMOVDQU  160(SI), Y13
	VPDPBUSD Y9, Y13, Y1
	VMOVDQU  288(SI), Y12
	VPDPBUSD Y9, Y12, Y2
	VMOVDQU  416(SI), Y13
	VPDPBUSD Y9, Y13, Y3
	VMOVDQU  544(SI), Y12
	VPDPBUSD Y9, Y12, Y4
	VMOVDQU  672(SI), Y13
	VPDPBUSD Y9, Y13, Y5
	VMOVDQU  800(SI), Y12
	VPDPBUSD Y9, Y12, Y6
	VMOVDQU  928(SI), Y13
	VPDPBUSD Y9, Y13, Y7
	VMOVDQU  64(SI), Y12
	VPDPBUSD Y10, Y12, Y0
	VMOVDQU  192(SI), Y13
	VPDPBUSD Y10, Y13, Y1
	VMOVDQU  320(SI), Y12
	VPDPBUSD Y10, Y12, Y2
	VMOVDQU  448(SI), Y13
	VPDPBUSD Y10, Y13, Y3
	VMOVDQU  576(SI), Y12
	VPDPBUSD Y10, Y12, Y4
	VMOVDQU  704(SI), Y13
	VPDPBUSD Y10, Y13, Y5
	VMOVDQU  832(SI), Y12
	VPDPBUSD Y10, Y12, Y6
	VMOVDQU  960(SI), Y13
	VPDPBUSD Y10, Y13, Y7
	VMOVDQU  96(SI), Y12
	VPDPBUSD Y11, Y12, Y0
	VMOVDQU  224(SI), Y13
	VPDPBUSD Y11, Y13, Y1
	VMOVDQU  352(SI), Y12
	VPDPBUSD Y11, Y12, Y2
	VMOVDQU  480(SI), Y13
	VPDPBUSD Y11, Y13, Y3
	VMOVDQU  608(SI), Y12
	VPDPBUSD Y11, Y12, Y4
	VMOVDQU  736(SI), Y13
	VPDPBUSD Y11, Y13, Y5
	VMOVDQU  864(SI), Y12
	VPDPBUSD Y11, Y12, Y6
	VMOVDQU  992(SI), Y13
	VPDPBUSD Y11, Y13, Y7
	VPHADDD    Y1, Y0, Y0
	VPHADDD    Y3, Y2, Y2
	VPHADDD    Y2, Y0, Y0
	VPHADDD    Y5, Y4, Y4
	VPHADDD    Y7, Y6, Y6
	VPHADDD    Y6, Y4, Y4
	VPERM2I128 $0x20, Y4, Y0, Y5
	VPERM2I128 $0x31, Y4, Y0, Y6
	VPADDD     Y6, Y5, Y5
	VPSUBD     Y14, Y5, Y5
	VMOVDQU    Y5, (R10)
	ADDQ       $32, R10
	ADDQ       $1024, SI
	SUBQ       $8, R11
	JMP        dotU8Block128x8VNNI_loop

dotU8Block128x8VNNI_done:
	VZEROUPPER
	RET

// func dotU8Block128x8(c []int8, e []int8, bias int32, out []int32)
// The AVX2 form of dotU8Block128x8VNNI (VPMADDUBSW+VPMADDWD+VPADDD).
TEXT ·dotU8Block128x8(SB), NOSPLIT, $0-80
	MOVQ         c_base+0(FP), SI
	MOVQ         e_base+24(FP), R8
	MOVL         bias+48(FP), AX
	MOVQ         out_base+56(FP), R10
	MOVQ         out_len+64(FP), R11
	VPCMPEQW     Y15, Y15, Y15
	VPSRLW       $15, Y15, Y15
	VMOVD        AX, X14
	VPBROADCASTD X14, Y14
	VMOVDQU      0(R8), Y8
	VMOVDQU      32(R8), Y9
	VMOVDQU      64(R8), Y10
	VMOVDQU      96(R8), Y11

dotU8Block128x8_loop:
	CMPQ  R11, $8
	JL    dotU8Block128x8_done
	VPXOR Y0, Y0, Y0
	VPXOR Y1, Y1, Y1
	VPXOR Y2, Y2, Y2
	VPXOR Y3, Y3, Y3
	VPXOR Y4, Y4, Y4
	VPXOR Y5, Y5, Y5
	VPXOR Y6, Y6, Y6
	VPXOR Y7, Y7, Y7
	VMOVDQU    0(SI), Y12
	VPMADDUBSW Y8, Y12, Y12
	VPMADDWD   Y15, Y12, Y12
	VPADDD     Y12, Y0, Y0
	VMOVDQU    128(SI), Y13
	VPMADDUBSW Y8, Y13, Y13
	VPMADDWD   Y15, Y13, Y13
	VPADDD     Y13, Y1, Y1
	VMOVDQU    256(SI), Y12
	VPMADDUBSW Y8, Y12, Y12
	VPMADDWD   Y15, Y12, Y12
	VPADDD     Y12, Y2, Y2
	VMOVDQU    384(SI), Y13
	VPMADDUBSW Y8, Y13, Y13
	VPMADDWD   Y15, Y13, Y13
	VPADDD     Y13, Y3, Y3
	VMOVDQU    512(SI), Y12
	VPMADDUBSW Y8, Y12, Y12
	VPMADDWD   Y15, Y12, Y12
	VPADDD     Y12, Y4, Y4
	VMOVDQU    640(SI), Y13
	VPMADDUBSW Y8, Y13, Y13
	VPMADDWD   Y15, Y13, Y13
	VPADDD     Y13, Y5, Y5
	VMOVDQU    768(SI), Y12
	VPMADDUBSW Y8, Y12, Y12
	VPMADDWD   Y15, Y12, Y12
	VPADDD     Y12, Y6, Y6
	VMOVDQU    896(SI), Y13
	VPMADDUBSW Y8, Y13, Y13
	VPMADDWD   Y15, Y13, Y13
	VPADDD     Y13, Y7, Y7
	VMOVDQU    32(SI), Y12
	VPMADDUBSW Y9, Y12, Y12
	VPMADDWD   Y15, Y12, Y12
	VPADDD     Y12, Y0, Y0
	VMOVDQU    160(SI), Y13
	VPMADDUBSW Y9, Y13, Y13
	VPMADDWD   Y15, Y13, Y13
	VPADDD     Y13, Y1, Y1
	VMOVDQU    288(SI), Y12
	VPMADDUBSW Y9, Y12, Y12
	VPMADDWD   Y15, Y12, Y12
	VPADDD     Y12, Y2, Y2
	VMOVDQU    416(SI), Y13
	VPMADDUBSW Y9, Y13, Y13
	VPMADDWD   Y15, Y13, Y13
	VPADDD     Y13, Y3, Y3
	VMOVDQU    544(SI), Y12
	VPMADDUBSW Y9, Y12, Y12
	VPMADDWD   Y15, Y12, Y12
	VPADDD     Y12, Y4, Y4
	VMOVDQU    672(SI), Y13
	VPMADDUBSW Y9, Y13, Y13
	VPMADDWD   Y15, Y13, Y13
	VPADDD     Y13, Y5, Y5
	VMOVDQU    800(SI), Y12
	VPMADDUBSW Y9, Y12, Y12
	VPMADDWD   Y15, Y12, Y12
	VPADDD     Y12, Y6, Y6
	VMOVDQU    928(SI), Y13
	VPMADDUBSW Y9, Y13, Y13
	VPMADDWD   Y15, Y13, Y13
	VPADDD     Y13, Y7, Y7
	VMOVDQU    64(SI), Y12
	VPMADDUBSW Y10, Y12, Y12
	VPMADDWD   Y15, Y12, Y12
	VPADDD     Y12, Y0, Y0
	VMOVDQU    192(SI), Y13
	VPMADDUBSW Y10, Y13, Y13
	VPMADDWD   Y15, Y13, Y13
	VPADDD     Y13, Y1, Y1
	VMOVDQU    320(SI), Y12
	VPMADDUBSW Y10, Y12, Y12
	VPMADDWD   Y15, Y12, Y12
	VPADDD     Y12, Y2, Y2
	VMOVDQU    448(SI), Y13
	VPMADDUBSW Y10, Y13, Y13
	VPMADDWD   Y15, Y13, Y13
	VPADDD     Y13, Y3, Y3
	VMOVDQU    576(SI), Y12
	VPMADDUBSW Y10, Y12, Y12
	VPMADDWD   Y15, Y12, Y12
	VPADDD     Y12, Y4, Y4
	VMOVDQU    704(SI), Y13
	VPMADDUBSW Y10, Y13, Y13
	VPMADDWD   Y15, Y13, Y13
	VPADDD     Y13, Y5, Y5
	VMOVDQU    832(SI), Y12
	VPMADDUBSW Y10, Y12, Y12
	VPMADDWD   Y15, Y12, Y12
	VPADDD     Y12, Y6, Y6
	VMOVDQU    960(SI), Y13
	VPMADDUBSW Y10, Y13, Y13
	VPMADDWD   Y15, Y13, Y13
	VPADDD     Y13, Y7, Y7
	VMOVDQU    96(SI), Y12
	VPMADDUBSW Y11, Y12, Y12
	VPMADDWD   Y15, Y12, Y12
	VPADDD     Y12, Y0, Y0
	VMOVDQU    224(SI), Y13
	VPMADDUBSW Y11, Y13, Y13
	VPMADDWD   Y15, Y13, Y13
	VPADDD     Y13, Y1, Y1
	VMOVDQU    352(SI), Y12
	VPMADDUBSW Y11, Y12, Y12
	VPMADDWD   Y15, Y12, Y12
	VPADDD     Y12, Y2, Y2
	VMOVDQU    480(SI), Y13
	VPMADDUBSW Y11, Y13, Y13
	VPMADDWD   Y15, Y13, Y13
	VPADDD     Y13, Y3, Y3
	VMOVDQU    608(SI), Y12
	VPMADDUBSW Y11, Y12, Y12
	VPMADDWD   Y15, Y12, Y12
	VPADDD     Y12, Y4, Y4
	VMOVDQU    736(SI), Y13
	VPMADDUBSW Y11, Y13, Y13
	VPMADDWD   Y15, Y13, Y13
	VPADDD     Y13, Y5, Y5
	VMOVDQU    864(SI), Y12
	VPMADDUBSW Y11, Y12, Y12
	VPMADDWD   Y15, Y12, Y12
	VPADDD     Y12, Y6, Y6
	VMOVDQU    992(SI), Y13
	VPMADDUBSW Y11, Y13, Y13
	VPMADDWD   Y15, Y13, Y13
	VPADDD     Y13, Y7, Y7
	VPHADDD    Y1, Y0, Y0
	VPHADDD    Y3, Y2, Y2
	VPHADDD    Y2, Y0, Y0
	VPHADDD    Y5, Y4, Y4
	VPHADDD    Y7, Y6, Y6
	VPHADDD    Y6, Y4, Y4
	VPERM2I128 $0x20, Y4, Y0, Y5
	VPERM2I128 $0x31, Y4, Y0, Y6
	VPADDD     Y6, Y5, Y5
	VPSUBD     Y14, Y5, Y5
	VMOVDQU    Y5, (R10)
	ADDQ       $32, R10
	ADDQ       $1024, SI
	SUBQ       $8, R11
	JMP        dotU8Block128x8_loop

dotU8Block128x8_done:
	VZEROUPPER
	RET

// func cpuRelax()
// One PAUSE: tells the core that this is a spin-wait, so a hyperthread
// sibling keeps its execution resources and the loop does not flood the
// memory pipeline.
TEXT ·cpuRelax(SB), NOSPLIT, $0-0
	PAUSE
	RET
