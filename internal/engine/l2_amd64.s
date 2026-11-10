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

// func dotQ8Block(c []int8, e []int16, out []int32)
TEXT ·dotQ8Block(SB), NOSPLIT, $0-72
	MOVQ c_base+0(FP), SI
	MOVQ e_base+24(FP), R8
	MOVQ e_len+32(FP), R9
	MOVQ out_base+48(FP), R10
	MOVQ out_len+56(FP), R11

qrow:
	CMPQ  R11, $0
	JE    qdone
	MOVQ  R8, DI
	MOVQ  R9, CX
	VPXOR Y0, Y0, Y0
	VPXOR Y1, Y1, Y1

qloop:
	CMPQ      CX, $32
	JL        qtail
	VPMOVSXBW (SI), Y2
	VPMOVSXBW 16(SI), Y3
	VPMADDWD  (DI), Y2, Y2
	VPMADDWD  32(DI), Y3, Y3
	VPADDD    Y2, Y0, Y0
	VPADDD    Y3, Y1, Y1
	ADDQ      $32, SI
	ADDQ      $64, DI
	SUBQ      $32, CX
	JMP       qloop

qtail:
	CMPQ      CX, $0
	JE        qreduce
	VPMOVSXBW (SI), Y2
	VPMADDWD  (DI), Y2, Y2
	VPADDD    Y2, Y0, Y0
	ADDQ      $16, SI
	ADDQ      $32, DI
	SUBQ      $16, CX
	JMP       qtail

qreduce:
	VPADDD       Y1, Y0, Y0
	VEXTRACTI128 $1, Y0, X1
	VPADDD       X1, X0, X0
	VPHADDD      X0, X0, X0
	VPHADDD      X0, X0, X0
	VMOVD        X0, (R10)
	ADDQ         $4, R10
	DECQ         R11
	JMP          qrow

qdone:
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
