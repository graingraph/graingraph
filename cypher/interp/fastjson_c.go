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
	if len(batch) == 0 {
		return []map[string]any{}, true
	}
	n := len(batch)
	// keep backing slices alive for the duration of the C call
	bufs := make([][]byte, n)
	ptrs := make([]unsafe.Pointer, n)
	lensGo := make([]C.size_t, n)
	for i, s := range batch {
		b := []byte(s)
		bufs[i] = b
		if len(b) > 0 {
			ptrs[i] = unsafe.Pointer(&b[0])
		} else {
			ptrs[i] = unsafe.Pointer(uintptr(0))
		}
		lensGo[i] = C.size_t(len(s))
	}
	// allocate C arrays for datas and lens
	datas := C.malloc(C.size_t(n) * C.size_t(unsafe.Sizeof(uintptr(0))))
	lensArr := C.malloc(C.size_t(n) * C.size_t(unsafe.Sizeof(C.size_t(0))))
	if datas == nil || lensArr == nil {
		return nil, false
	}
	defer C.free(datas)
	defer C.free(lensArr)
	// fill C arrays
	for i := 0; i < n; i++ {
		*(*unsafe.Pointer)(unsafe.Pointer(uintptr(uintptr(datas)) + uintptr(i)*unsafe.Sizeof(uintptr(0)))) = ptrs[i]
		*(*C.size_t)(unsafe.Pointer(uintptr(uintptr(lensArr)) + uintptr(i)*unsafe.Sizeof(C.size_t(0)))) = lensGo[i]
	}
	// allocate output arrays
	keysArr := C.malloc(C.size_t(n) * C.size_t(unsafe.Sizeof(uintptr(0))))
	valsArr := C.malloc(C.size_t(n) * C.size_t(unsafe.Sizeof(uintptr(0))))
	countsArr := C.malloc(C.size_t(n) * C.size_t(unsafe.Sizeof(C.size_t(0))))
	if keysArr == nil || valsArr == nil || countsArr == nil {
		return nil, false
	}
	defer C.free(keysArr)
	defer C.free(valsArr)
	defer C.free(countsArr)
	if C.fast_decode_batch_flat((**C.uchar)(unsafe.Pointer(datas)), (*C.size_t)(unsafe.Pointer(lensArr)), C.size_t(n), (***C.char)(unsafe.Pointer(keysArr)), (**C.FlatValue)(unsafe.Pointer(valsArr)), (*C.size_t)(unsafe.Pointer(countsArr))) == 0 {
		return nil, false
	}
	results := make([]map[string]any, n)
	for i := 0; i < n; i++ {
		keysPtr := *(***C.char)(unsafe.Pointer(uintptr(uintptr(keysArr)) + uintptr(i)*unsafe.Sizeof(uintptr(0))))
		valsPtr := *(* *C.FlatValue)(unsafe.Pointer(uintptr(uintptr(valsArr)) + uintptr(i)*unsafe.Sizeof(uintptr(0))))
		count := *(*C.size_t)(unsafe.Pointer(uintptr(uintptr(countsArr)) + uintptr(i)*unsafe.Sizeof(C.size_t(0))))
		if keysPtr == nil || valsPtr == nil {
			C.fast_free_flat(nil, nil, 0)
			return nil, false
		}
		m := make(map[string]any, int(count))
		keysSlice := (*[1 << 30]*C.char)(unsafe.Pointer(keysPtr))
		valsPtrRaw := unsafe.Pointer(valsPtr)
		for j := 0; j < int(count); j++ {
			key := C.GoString(keysSlice[j])
			typ := *(*uint8)(unsafe.Pointer(uintptr(valsPtrRaw) + uintptr(j)*16))
			valPtr := unsafe.Pointer(uintptr(valsPtrRaw) + uintptr(j)*16 + 8)
			switch typ {
			case 0:
				m[key] = nil
			case 1:
				bv := *(*int32)(valPtr)
				m[key] = bv != 0
			case 2:
				v := *(*int64)(valPtr)
				m[key] = int64(v)
			case 3:
				v := *(*float64)(valPtr)
				m[key] = float64(v)
			case 4:
				ptr := *(*unsafe.Pointer)(valPtr)
				if ptr != nil {
					m[key] = C.GoString((*C.char)(ptr))
				} else {
					m[key] = ""
				}
			default:
				C.fast_free_flat(keysPtr, valsPtr, count)
				return nil, false
			}
		}
		C.fast_free_flat(keysPtr, valsPtr, count)
		results[i] = m
	}
	return results, true
}

