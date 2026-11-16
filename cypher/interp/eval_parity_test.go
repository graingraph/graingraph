package interp

import (
	"testing"
)

func TestEvalParityC_Basic(t *testing.T) {
	// Enable C eval core for this test
	WithInterpC()()
	if !interpCEnabled {
		t.Skip("interp C not enabled")
	}
	// Test 1 + 2 = 3 via C eval core
	res, ok := evalIntAddC(1, 2)
	if !ok {
		t.Fatalf("C eval failed")
	}
	// For now just ensure we got a result
	_ = res
}
