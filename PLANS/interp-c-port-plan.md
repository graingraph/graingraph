# Interpreter hot paths – C port plan

## Goal
Move the hottest interpreter loops out of Go into C via cgo while keeping the Go API, parser/analyzer, and TCK intact. First target: expression evaluation and streaming aggregation.

## Candidates from profiling
* `cypher/interp/eval.go` – `eval.Expr` / `eval.Value` – called per row, per expression. Hot for FILTER, WHERE, RETURN projections.
* `cypher/interp/stream.go` – `mapStage`, `aggregateStage`, `topN` – tight loops over `[]row`.
* `cypher/interp/project.go` – `finishProjectionRows` – allocation heavy.
* `cypher/interp/funcs.go` – temporal / numeric helpers.

## Approach
1. **Profile baseline**
   * Run `BenchmarkQueries` and `BenchmarkVectorSearch` with `-cpuprofile`.
   * Generate flamegraph, identify top 5 Go functions.
2. **Bytecode serialization**
   * Already have `syntax.Statement` AST. Serialize expression subtrees to a flat opcode array + constant pool.
   * `c/interp_expr.h` / `c/interp_expr.c` – eval loop in C, arena allocator per statement.
3. **cgo boundary**
   * `cypher/interp/interp_c.go` `//go:build cgo`
   * `func evalExprC(opcodes []byte, consts []any, vars map[string]any) any`
   * Opt-in via `WithInterpC()` flag, parity tests against Go eval.
4. **Streaming aggregation**
   * Port `aggregateStage` and `topN` heap to C with pre-allocated ring buffers.
   * Keep Go orchestration, call C for the tight inner loop.
5. **Validation**
   * Run TCK `TestTCK_ValidScenariosPass` and `TestTCK_CompileTimeErrors` with flag on/off.
   * Benchmark `BenchmarkQueries/filter` and `BenchmarkQueries/aggregate` before/after.

## Deliverables
* `c/interp_expr.c/.h`
* `cypher/interp/interp_c.go` + stub
* `PLANS/interp-c-baseline.json` – before numbers
* Parity tests in `cypher/interp/eval_test.go`

## Risk mitigations
* Keep AST in Go, only eval loop moves.
* Re-enter Go for complex functions that need error messages.
* Build tag kept off by default; pure Go builds remain `CGO_ENABLED=0`.

Next step: run profiling with larger query set and generate flamegraph to confirm eval.go is top.
