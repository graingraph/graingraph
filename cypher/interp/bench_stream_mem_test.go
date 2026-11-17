package interp

import (
	"testing"
)

type memRow struct {
	a, b, c, d, e int64
}

func BenchmarkStreamMemAgg(b *testing.B) {
	const rows = 1_000_000
	data := make([]memRow, rows)
	for i := range data {
		data[i] = memRow{int64(i), int64(i*2), int64(i*3), int64(i*4), int64(i*5)}
	}
	b.ResetTimer()
	var sum int64
	b.Run("map-agg", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sum = 0
			for _, r := range data {
				// simulate expression evaluation: (a + b) * c - d + e
				v := (r.a + r.b) * r.c - r.d + r.e
				sum += v
			}
		}
	})
	_ = sum
}
