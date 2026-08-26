#include "textflag.h"

TEXT ·seven(SB), NOSPLIT, $0-8
	MOVD $7, R0
	MOVD R0, ret+0(FP)
	RET
