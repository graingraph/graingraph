//go:build cgo
// +build cgo

package vector

/*
#cgo CFLAGS: -I${SRCDIR}/../../c
#cgo LDFLAGS: -L${SRCDIR}/../../c -lvector -lm
#include "vector_kernels.h"
#include <stdlib.h>
*/
import "C"
import "unsafe"

func dot32C(a, b []float32) float32 {
	if len(a) == 0 {
		return 0
	}
	return float32(C.dot32((*C.float)(unsafe.Pointer(&a[0])), (*C.float)(unsafe.Pointer(&b[0])), C.size_t(len(a))))
}

func squaredL2C(a, b []float32) float32 {
	if len(a) == 0 {
		return 0
	}
	return float32(C.squared_l2((*C.float)(unsafe.Pointer(&a[0])), (*C.float)(unsafe.Pointer(&b[0])), C.size_t(len(a))))
}

func norm32C(a []float32) float32 {
	if len(a) == 0 {
		return 0
	}
	return float32(C.norm32((*C.float)(unsafe.Pointer(&a[0])), C.size_t(len(a))))
}

func normalize32C(a []float32) bool {
	if len(a) == 0 {
		return false
	}
	return C.normalize32((*C.float)(unsafe.Pointer(&a[0])), C.size_t(len(a))) != 0
}
