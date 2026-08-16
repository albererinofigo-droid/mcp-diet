//go:build !race

package prune_test

// raceEnabled reports whether the test binary was built with -race. The
// detector adds an order of magnitude of overhead, which makes wall-clock
// budgets meaningless, so the latency tests skip under it.
const raceEnabled = false
