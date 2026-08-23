#include "textflag.h"
#include "funcdata.h"

// runtime.stackmap: n int32; nbit int32; bytedata [1]byte
// locals area is 24 bytes (3 slots); the pointer lives in slot 2.
DATA ·livemap+0(SB)/4, $1
DATA ·livemap+4(SB)/4, $3
DATA ·livemap+8(SB)/4, $4
GLOBL ·livemap(SB), RODATA, $12

// func spikeLive() int
TEXT ·spikeLive(SB), $32-8
	FUNCDATA $FUNCDATA_LocalsPointerMaps, ·livemap(SB)
	MOVD	$0, obj-8(SP)
	CALL	·mk(SB)
	MOVD	8(RSP), R0
	MOVD	R0, obj-8(SP)
	MOVD	$0, R0
	MOVD	$0, 8(RSP)		// drop the only other reference
	PCDATA	$1, $0
	CALL	·gcNow(SB)
	MOVD	obj-8(SP), R0
	MOVD	R0, 8(RSP)
	PCDATA	$1, $0
	CALL	·check(SB)
	MOVD	$0, ret+0(FP)
	RET

// func spikeDead() int
TEXT ·spikeDead(SB), $32-8
	NO_LOCAL_POINTERS
	MOVD	$0, obj-8(SP)
	CALL	·mk(SB)
	MOVD	8(RSP), R0
	MOVD	R0, obj-8(SP)
	MOVD	$0, R0
	MOVD	$0, 8(RSP)
	CALL	·gcNow(SB)
	MOVD	$0, ret+0(FP)
	RET

// A deliberately wrong map: claims slot 0 (an outgoing-argument slot that
// holds a small integer) is a pointer.
DATA ·bogusmap+0(SB)/4, $1
DATA ·bogusmap+4(SB)/4, $3
DATA ·bogusmap+8(SB)/4, $1
GLOBL ·bogusmap(SB), RODATA, $12

// func spikeBogus() int
TEXT ·spikeBogus(SB), $32-8
	FUNCDATA $FUNCDATA_LocalsPointerMaps, ·bogusmap(SB)
	MOVD	$1234, R0
	MOVD	R0, 8(RSP)
	PCDATA	$1, $0
	CALL	·gcNow(SB)
	MOVD	$0, ret+0(FP)
	RET

// Two bitmaps, selected per-PC by PCDATA $PCDATA_StackMapIndex.
// index 0: slot 2 holds a pointer.  index 1: no pointers.
DATA ·multimap+0(SB)/4, $2
DATA ·multimap+4(SB)/4, $3
DATA ·multimap+8(SB)/1, $4
DATA ·multimap+9(SB)/1, $0
GLOBL ·multimap(SB), RODATA, $10

// func spikeMulti() int
TEXT ·spikeMulti(SB), $32-8
	FUNCDATA $FUNCDATA_LocalsPointerMaps, ·multimap(SB)
	MOVD	$0, obj-8(SP)
	PCDATA	$1, $1
	CALL	·mk(SB)
	MOVD	8(RSP), R0
	MOVD	R0, obj-8(SP)
	MOVD	$0, R0
	MOVD	$0, 8(RSP)
	PCDATA	$1, $0
	CALL	·gcNow(SB)
	PCDATA	$1, $0
	CALL	·phase1(SB)
	PCDATA	$1, $1
	CALL	·gcNow(SB)
	PCDATA	$1, $1
	CALL	·phase2(SB)
	MOVD	$0, ret+0(FP)
	RET
