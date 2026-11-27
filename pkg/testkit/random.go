package testkit

import (
	"os"
	"strconv"
	"testing"
	"time"
)

// RandomRun says how many cases a test of generated cases runs, and from
// which seed. A normal run is modest and from a fixed seed, so it is the
// same every time. MANDATE_RANDOM_CASES and MANDATE_RANDOM_SEED (a number,
// or "random") make a longer hunt. Cases are made one after another from
// the seed, so a failure is met again only with both the seed and at least
// as many cases; both are logged.
func RandomRun(t testing.TB, defaultCases int) (cases int, seed uint64) {
	t.Helper()
	cases, seed = defaultCases, 1
	if text := os.Getenv("MANDATE_RANDOM_CASES"); text != "" {
		n, err := strconv.Atoi(text)
		if err != nil || n < 1 {
			t.Fatalf("MANDATE_RANDOM_CASES must be a positive number, not %q", text)
		}
		cases = n
	}
	switch text := os.Getenv("MANDATE_RANDOM_SEED"); text {
	case "":
	case "random":
		seed = uint64(time.Now().UnixNano()) //nolint:gosec // any seed will do
	default:
		n, err := strconv.ParseUint(text, 10, 64)
		if err != nil {
			t.Fatalf("MANDATE_RANDOM_SEED must be a number or \"random\", not %q", text)
		}
		seed = n
	}
	t.Logf("%d cases from seed %d (MANDATE_RANDOM_CASES=%d MANDATE_RANDOM_SEED=%d runs them again)", cases, seed, cases, seed)
	return cases, seed
}
