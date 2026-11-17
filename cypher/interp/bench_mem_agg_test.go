package interp

import (
	"testing"
)

func BenchmarkMemAgg(b *testing.B) {
	// synthetic rows: 1M rows, each with 5 int values
	const rows = 1_000_000
	const cols = 5
	data := make([][cols]int64, rows)
	for i := range data {
		for j := 0; j < cols; j++ {
			data[i][j] = int64(i*cols + j)
		}
	}
	b.ResetTimer()
	var sum int64
	b.Run("sum", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			sum = 0
			for _, r := range data {
				sum += r[0] + r[1] + r[2] + r[3] + r[4]
			}
		}
	})
	_ = sum
}
