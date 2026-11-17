package interp
import (
    "fmt"
    "testing"
)
func TestDebugFastC(t *testing.T) {
    s := `{"category":"Cat3","id":1003,"incorporated":1983,"name":"Company 1003 Ltd","score":3,"status":"Active"}`
    m, ok := fastDecodeObjectC(s)
    fmt.Printf("ok=%v, m=%v\n", ok, m)
    if !ok {
        t.Fatalf("failed")
    }
}
