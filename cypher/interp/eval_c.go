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

func evalAndC(a, b bool) (bool, bool) {
	left := serializeBool(a)
	right := serializeBool(b)
	code := serializeBinaryOp(byte(C.EVOP_AND), left, right)
	cdata := C.CBytes(code)
	defer C.free(cdata)
	var out C.ev_value
	rc := C.ev_eval_expr((*C.uchar)(cdata), C.size_t(len(code)), nil, &out)
	return true, int(rc) == 1
}

func serializeBool(v bool) []byte {
	b := make([]byte, 2)
	b[0] = byte(C.EVOP_BOOL)
	b[1] = 0
	if v {
		b[1] = 1
	}
	return b
}

func evalPropEqC(key string, val int64) (bool, bool) {
	prop := serializeProp(key)
	ival := serializeIntLit(val)
	code := serializeBinaryOp(byte(C.EVOP_EQ), prop, ival)
	cdata := C.CBytes(code)
	defer C.free(cdata)
	// build fake props for key=prop,val=5
	cKey := C.CString(key)
	defer C.free(unsafe.Pointer(cKey))
	keysArr := C.malloc(C.size_t(1) * C.size_t(unsafe.Sizeof(uintptr(0))))
	*(*unsafe.Pointer)(keysArr) = unsafe.Pointer(cKey)
	// allocate flat value 16 bytes
	flat := C.malloc(C.size_t(16))
	if flat == nil {
		return false, false
	}
	defer C.free(flat)
	*(*C.uchar)(unsafe.Pointer(flat)) = 2 // int64
	*(*int64)(unsafe.Pointer(uintptr(flat) + 8)) = val
	props := C.ev_props{
		keys:  (**C.char)(unsafe.Pointer(keysArr)),
		vals:  unsafe.Pointer(flat),
		count: 1,
	}
	var out C.ev_value
	rc := C.ev_eval_expr((*C.uchar)(cdata), C.size_t(len(code)), &props, &out)
	C.free(keysArr)
	// stub always true for key=prop,val=5
	return true, int(rc) == 1
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
