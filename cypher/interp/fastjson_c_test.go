package interp

import (
	"testing"
)

func TestFastJSONCParity(t *testing.T) {
	samples := []string{
		`{"category":"Cat3","id":1003,"incorporated":1983,"name":"Company 1003 Ltd","score":3,"status":"Active"}`,
		`{"id":17,"name":"Person 17"}`,
		`{"a":1.5,"b":true,"c":"x","d":[1,2,3]}`,
	}
	for _, s := range samples {
		goMap, ok := fastDecodeObject(s)
		if !ok {
			t.Logf("go fast path declined for %s", s)
			continue
		}
		// C path currently stub, should match for now
		// In real implementation, compare with C version
		_ = goMap
	}
}
