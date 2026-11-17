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

type decodedProps struct {
	keys  **C.char
	vals  *C.FlatValue
	count C.size_t
}

func (d *decodedProps) release() {
	if d != nil && d.keys != nil && d.vals != nil {
		C.fast_free_flat(d.keys, d.vals, d.count)
	}
}

func (d *decodedProps) get(name string) (any, bool) {
	if d == nil || d.count == 0 {
		return nil, false
	}
	// linear scan – property objects are small
	keysSlice := (*[1 << 30]*C.char)(unsafe.Pointer(d.keys))
	for i := 0; i < int(d.count); i++ {
		key := C.GoString(keysSlice[i])
		if key == name {
			valsPtr := unsafe.Pointer(uintptr(unsafe.Pointer(d.vals)) + uintptr(i)*16)
			typ := *(*uint8)(unsafe.Pointer(uintptr(valsPtr)))
			valPtr := unsafe.Pointer(uintptr(valsPtr) + 8)
			switch typ {
			case 0:
				return nil, true
			case 1:
				bv := *(*int32)(valPtr)
				return bv != 0, true
			case 2:
				v := *(*int64)(valPtr)
				return int64(v), true
			case 3:
				v := *(*float64)(valPtr)
				return float64(v), true
			case 4:
				ptr := *(*unsafe.Pointer)(valPtr)
				if ptr != nil {
					return C.GoString((*C.char)(ptr)), true
				}
				return "", true
			}
		}
	}
	return nil, false
}

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
func fastDecodeObjectsHandles(batch []string) ([]*decodedProps, bool) {
	if len(batch) == 0 {
		return []*decodedProps{}, true
	}
	n := len(batch)
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
	datas := C.malloc(C.size_t(n) * C.size_t(unsafe.Sizeof(uintptr(0))))
	lensArr := C.malloc(C.size_t(n) * C.size_t(unsafe.Sizeof(C.size_t(0))))
	if datas == nil || lensArr == nil {
		return nil, false
	}
	defer C.free(datas)
	defer C.free(lensArr)
	for i := 0; i < n; i++ {
		*(*unsafe.Pointer)(unsafe.Pointer(uintptr(uintptr(datas)) + uintptr(i)*unsafe.Sizeof(uintptr(0)))) = ptrs[i]
		*(*C.size_t)(unsafe.Pointer(uintptr(uintptr(lensArr)) + uintptr(i)*unsafe.Sizeof(C.size_t(0)))) = lensGo[i]
	}
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
	results := make([]*decodedProps, n)
	for i := 0; i < n; i++ {
		keysPtr := *(***C.char)(unsafe.Pointer(uintptr(uintptr(keysArr)) + uintptr(i)*unsafe.Sizeof(uintptr(0))))
		valsPtr := *(* *C.FlatValue)(unsafe.Pointer(uintptr(uintptr(valsArr)) + uintptr(i)*unsafe.Sizeof(uintptr(0))))
		count := *(*C.size_t)(unsafe.Pointer(uintptr(uintptr(countsArr)) + uintptr(i)*unsafe.Sizeof(C.size_t(0))))
		if keysPtr == nil || valsPtr == nil {
			// free already allocated
			for j := 0; j < i; j++ {
				results[j].release()
			}
			return nil, false
		}
		results[i] = &decodedProps{
			keys:  (**C.char)(unsafe.Pointer(keysPtr)),
			vals:  valsPtr,
			count: count,
		}
	}
	return results, true
}

// fastDecodeObjectsC is a compatibility shim for existing callers.
func fastDecodeObjectsC(batch []string) ([]map[string]any, bool) {
	handles, ok := fastDecodeObjectsHandles(batch)
	if !ok {
		return nil, false
	}
	out := make([]map[string]any, len(handles))
	for i, h := range handles {
		if h == nil {
			out[i] = map[string]any{}
			continue
		}
		m := make(map[string]any)
		keysSlice := (*[1 << 30]*C.char)(unsafe.Pointer(h.keys))
		valsPtr := unsafe.Pointer(h.vals)
		for j := 0; j < int(h.count); j++ {
			key := C.GoString(keysSlice[j])
			typ := *(*uint8)(unsafe.Pointer(uintptr(valsPtr) + uintptr(j)*16))
			valPtr := unsafe.Pointer(uintptr(valsPtr) + uintptr(j)*16 + 8)
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
			}
		}
		h.release()
		out[i] = m
	}
	return out, true
}

