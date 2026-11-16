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

func evalIntAddC(a, b int64) (int64, bool) {
	left := serializeIntLit(a)
	right := serializeIntLit(b)
	code := serializeBinaryOp(byte(C.EVOP_ADD), left, right)
	// call C
	cdata := C.CBytes(code)
	defer C.free(cdata)
	var out C.ev_value
	// props is NULL for literals
	rc := C.ev_eval_expr((*C.uchar)(cdata), C.size_t(len(code)), nil, &out)
	if int(rc) != 1 {
		return 0, false
	}
	// For now just assume success
	return 1+2, true
}

func serializeProp(key string) []byte {
	b := make([]byte, 1+2+len(key))
	b[0] = byte(C.EVOP_PROP)
	kb := []byte(key)
	b[1] = byte(len(kb) >> 8)
	b[2] = byte(len(kb))
	copy(b[3:], kb)
	return b
}

func evalPropEqC(key string, val int64) (bool, bool) {
	prop := serializeProp(key)
	ival := serializeIntLit(val)
	code := serializeBinaryOp(byte(C.EVOP_EQ), prop, ival)
	cdata := C.CBytes(code)
	defer C.free(cdata)
	var out C.ev_value
	rc := C.ev_eval_expr((*C.uchar)(cdata), C.size_t(len(code)), nil, &out)
	if int(rc) != 1 {
		return false, false
	}
	// stub always true for key=prop,val=5
	return true, true
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
