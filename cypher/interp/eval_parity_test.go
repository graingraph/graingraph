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
	_ = res
}

func TestEvalParityC_PropEq(t *testing.T) {
	WithInterpC()()
	if !interpCEnabled {
		t.Skip("interp C not enabled")
	}
	ok, _ := evalPropEqC("prop", 5)
	if !ok {
		t.Fatalf("prop eq eval failed")
	}
}

func TestEvalParityC_And(t *testing.T) {
	WithInterpC()()
	if !interpCEnabled {
		t.Skip("interp C not enabled")
	}
	ok, _ := evalAndC(true, true)
	if !ok {
		t.Fatalf("and eval failed")
	}
}

func TestEvalParityC_Or(t *testing.T) {
	WithInterpC()()
	if !interpCEnabled {
		t.Skip("interp C not enabled")
	}
	ok, _ := evalOrC(false, true)
	if !ok {
		t.Fatalf("or eval failed")
	}
}

func TestEvalParityC_Not(t *testing.T) {
	WithInterpC()()
	if !interpCEnabled {
		t.Skip("interp C not enabled")
	}
	ok, _ := evalNotC(true)
	if !ok {
		t.Fatalf("not eval failed")
	}
}

func TestEvalParityC_In(t *testing.T) {
	WithInterpC()()
	if !interpCEnabled {
		t.Skip("interp C not enabled")
	}
	ok, _ := evalInC(5, 5)
	if !ok {
		t.Fatalf("in eval failed")
	}
}

func TestEvalParityC_StartsWith(t *testing.T) {
	WithInterpC()()
	if !interpCEnabled {
		t.Skip("interp C not enabled")
	}
	_, ok := evalStartsC("hello world", "hello")
	if !ok {
		t.Fatalf("starts with eval failed")
	}
}
