package interp

import (
	"testing"
)

func BenchmarkDecodePropsBatch(b *testing.B) {
	// reuse the same fixtures as BenchmarkDecodeProps
	// create 256 objects per batch
	const batchSize = 256
	fixtures := []struct{
		name string
		json string
	}{
		{"company6", `{"name":"Acme","employees":123}`},
		{"person2", `{"id":1,"name":"Bob"}`},
		{"mixed", `{"a":1,"b":true,"c":3.14,"d":"x"}`},
	}
	batches := make([][]string, len(fixtures))
	for i, f := range fixtures {
		batches[i] = make([]string, batchSize)
		for j := range batches[i] {
			batches[i][j] = f.json
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, batch := range batches {
			_, ok := fastDecodeObjectsC(batch)
			if !ok {
				b.Fatal("decode failed")
			}
		}
	}
}
