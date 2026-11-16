//go:build !cgo
// +build !cgo

package vector

func dot32C(a, b []float32) float32 {
	panic("vector C kernels not available: built without cgo")
}
func squaredL2C(a, b []float32) float32 {
	panic("vector C kernels not available: built without cgo")
}
func norm32C(a []float32) float32 {
	panic("vector C kernels not available: built without cgo")
}
func normalize32C(a []float32) bool {
	panic("vector C kernels not available: built without cgo")
}
