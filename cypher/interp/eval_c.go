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

// evalExprC is a stub bridge to the C eval core.
// It will be expanded to walk syntax.Expr and call ev_eval_expr.
func evalExprC(expr interface{}, props *decodedProps) (any, bool) {
	if !interpCEnabled {
		return nil, false
	}
	// Stub: not implemented yet
	_ = expr
	_ = props
	return nil, false
}
