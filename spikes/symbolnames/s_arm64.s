#include "textflag.h"

DATA ·leaf(SB)/8, $42
GLOBL ·leaf(SB), RODATA|NOPTR, $8

// a data word holding the address of another symbol (relocation)
DATA ·ref(SB)/8, $·leaf(SB)
GLOBL ·ref(SB), RODATA, $8

// compiler-shaped symbol names
DATA type·main·Obj(SB)/8, $7
GLOBL type·main·Obj(SB), RODATA|NOPTR, $8

TEXT ·target(SB), NOSPLIT, $0-8
	MOVD	·leaf(SB), R0
	MOVD	R0, ret+0(FP)
	RET

TEXT ·ptrdata(SB), NOSPLIT, $0-8
	MOVD	·ref(SB), R0
	MOVD	R0, ret+0(FP)
	RET
