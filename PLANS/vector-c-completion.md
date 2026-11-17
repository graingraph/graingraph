# Vector kernels C port – completion

## Status
Done on branch `port/c-rewrite`. Parity verified, buildable, opt-in via `vector.WithVectorC()`.

## Changes
* `c/vector_kernels.h` / `c/vector_kernels.c` – scalar 8-accumulator dot / squared_l2 / norm / normalize kernels.
* `build/libvector.a` / `c/libvector.a` – built with `gcc -c` + `ar`.
* `cypher/vector/kernels.go` – `vectorCEnabled` flag + `WithVectorC()`; `Dot32`, `SquaredL2`, `Norm32`, `Normalize32` delegate to C when enabled.
* `cypher/vector/kernels_c.go` – cgo wrapper, `//go:build cgo`, LDFLAGS `-lvector -lm`.
* `cypher/vector/kernels_c_stub.go` – `//go:build !cgo` stub.

## Verification
* `TestKernelsMatchReference` passes with CGO_ENABLED=1.
* Benchmarks on Apple M5 Max, dim 384, 20k vectors:
  * Baseline Go `BenchmarkDot32`: ~1.31 ms/op
  * C opt-in `BenchmarkDot32C`: ~1.30 ms/op
* `BenchmarkVectorSearch/n=100000`:
  * CGO_ENABLED=0: 2.47 ms/op, cold decode 374 ms
  * CGO_ENABLED=1 with WithVectorC: 2.58 ms/op, cold decode 376 ms

Result: parity correct, no regression. C scalar loop overhead ~cgo call cost for current dimensions.

## Next
Move to interpreter hot paths – pattern matching / walk, property decoding, and fastjson production enable per `PLANS/hybrid-c-go.md`.
