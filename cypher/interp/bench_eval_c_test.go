package interp

import (
	"testing"
)

func BenchmarkEvalExprC(b *testing.B) {
	WithInterpC()()
	if !interpCEnabled {
		b.Skip("interp C not enabled")
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = evalPropEqC("prop", 5)
	}
}
