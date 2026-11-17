//go:build !cgo
// +build !cgo

package interp

func fastDecodeObjectC(s string) (map[string]any, bool) {
	return nil, false
}
