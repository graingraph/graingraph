//go:build cgo
// +build cgo

package interp

/*
#cgo CFLAGS: -I${SRCDIR}/../../c
#cgo LDFLAGS: -L${SRCDIR}/../../c -lfastjson
#include "fastjson.h"
#include <stdlib.h>
*/
import "C"
import (
	"unsafe"
)

func fastDecodeObjectC(s string) (map[string]any, bool) {
	if len(s) == 0 {
		return nil, false
	}
	// avoid C.CString allocation; pass bytes directly
	b := []byte(s)
	data := unsafe.Pointer(&b[0])
	var keys **C.char
	var vals *C.FlatValue
	var count C.size_t
	if C.fast_decode_object_flat_from_bytes((*C.uchar)(unsafe.Pointer(data)), C.size_t(len(s)), &keys, &vals, &count) == 0 {
		return nil, false
	}
	m := make(map[string]any, int(count))
	keysSlice := (*[1 << 30]*C.char)(unsafe.Pointer(keys))
	valsPtr := unsafe.Pointer(vals)
	for i := 0; i < int(count); i++ {
		key := C.GoString(keysSlice[i])
		// read FlatValue struct manually
		// first byte is typ
		typ := *(*uint8)(unsafe.Pointer(uintptr(valsPtr) + uintptr(i)*16))
		// union starts at offset 8
		valPtr := unsafe.Pointer(uintptr(valsPtr) + uintptr(i)*16 + 8)
		switch typ {
		case 0: // JV_NULL
			m[key] = nil
		case 1: // JV_BOOL
			bv := *(*int32)(valPtr)
			m[key] = bv != 0
		case 2: // JV_INT64
			v := *(*int64)(valPtr)
			m[key] = int64(v)
		case 3: // JV_DOUBLE
			v := *(*float64)(valPtr)
			m[key] = float64(v)
		case 4: // JV_STRING
			ptr := *(*unsafe.Pointer)(valPtr)
			if ptr != nil {
				m[key] = C.GoString((*C.char)(ptr))
			} else {
				m[key] = ""
			}
		default:
			C.fast_free_flat(keys, vals, count)
			return nil, false
		}
	}
	C.fast_free_flat(keys, vals, count)
	return m, true
}

// fastDecodeObjectsC decodes a batch of JSON objects in one cgo call.
func fastDecodeObjectsC(batch []string) ([]map[string]any, bool) {
	// TODO: full batch implementation with C arrays of pointers/lens.
	// For now fall back to per-object decode to keep parity.
	results := make([]map[string]any, len(batch))
	for i, s := range batch {
		m, ok := fastDecodeObjectC(s)
		if !ok {
			return nil, false
		}
		results[i] = m
	}
	return results, true
}

