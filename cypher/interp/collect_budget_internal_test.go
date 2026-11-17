package interp

import (
	"strings"
	"testing"
)

// collect() obeys the same list-length budget as range(), list concatenation
// and replace(): the statement fails with ValueTooLarge, it is never truncated.
// Both aggregation paths need the check: the streaming accumulator and the
// materialised aggregate used when streaming is off.
func TestCollectHonoursListBudget(t *testing.T) {
	old := maxListLen
	maxListLen = 50
	defer func() { maxListLen = old }()

	elems := func(n int) map[string]any {
		xs := make([]any, n)
		for i := range xs {
			xs[i] = int64(i)
		}
		return map[string]any{"xs": xs}
	}
	for _, streaming := range []bool{true, false} {
		streamDisabled = !streaming
		db := richGraph(t, 1, 10, 0)

		_, errText := runOrdered(t, db, "UNWIND $xs AS x RETURN collect(x) AS xs", elems(51))
		if !strings.Contains(errText, "list longer than the limit of 50 elements") {
			t.Errorf("streaming=%v: 51 elements: got error %q", streaming, errText)
		}
		rows, errText := runOrdered(t, db, "UNWIND $xs AS x RETURN size(collect(x)) AS n", elems(50))
		if errText != "" || len(rows) != 2 {
			t.Errorf("streaming=%v: exactly at the limit must succeed: rows=%v err=%q", streaming, rows, errText)
		}
		_, errText = runOrdered(t, db, "UNWIND $xs AS x RETURN count(x) AS n, sum(x) AS s", elems(51))
		if errText != "" {
			t.Errorf("streaming=%v: other aggregates are not list budgets: %q", streaming, errText)
		}
	}
	streamDisabled = false
}
