//go:build cgo
// +build cgo

package interp

/*
#cgo CFLAGS: -I${SRCDIR}/../../c
#cgo LDFLAGS: -L${SRCDIR}/../../c -levalcore
#include "eval_core.h"
#include <stdlib.h>
*/
import "C"
import (
	"unsafe"
	"github.com/graingraph/graingraph/cypher/syntax"
)

var interpCEnabled bool

// WithInterpC enables the C eval core for benchmarking.
func WithInterpC() func() {
	return func() { interpCEnabled = true }
}

func serializeIntLit(v int64) []byte {
	b := make([]byte, 1+8)
	b[0] = byte(C.EVOP_INT64)
	// little endian
	for i := 0; i < 8; i++ {
		b[1+i] = byte(v >> (i * 8))
	}
	return b
}

func serializeBinaryOp(op byte, left, right []byte) []byte {
	out := make([]byte, 0, len(left)+len(right)+1)
	out = append(out, left...)
	out = append(out, right...)
	out = append(out, op)
	return out
}

// evalExprC is a stub bridge to the C eval core.
// It will be expanded to walk syntax.Expr and call ev_eval_expr.
func evalExprC(expr interface{}, props *decodedProps) (any, bool) {
	if !interpCEnabled {
		return nil, false
	}
	_ = props
	// For now only handle simple integer literals and binary add
	// Real implementation would walk syntax.Expr
	return nil, false
}
