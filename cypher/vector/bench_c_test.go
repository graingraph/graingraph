package vector

import (
	"math/rand"
	"testing"
)

func init() {
	_ = WithVectorC()
}

func BenchmarkDot32C(b *testing.B) {
	r := rand.New(rand.NewSource(2))
	const dim, n = 384, 20000
	flat := make([]float32, dim*n)
	for i := range flat {
		flat[i] = float32(r.NormFloat64())
	}
	q := make([]float32, dim)
	for i := range q {
		q[i] = float32(r.NormFloat64())
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var best float32 = -1e9
		for k := 0; k < n; k++ {
			if s := Dot32(q, flat[k*dim:(k+1)*dim]); s > best {
				best = s
			}
		}
	}
}
