package events

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPlainDecimal(t *testing.T) {
	for in, want := range map[string]string{
		"5e-7": "0.0000005", "1.5E+3": "1500", "-2.5e-2": "-0.025", "1e+21": "1000000000000000000000",
		"12.34e1": "123.4", "0e5": "0", "3": "3", "0.25": "0.25", "7e0": "7",
	} {
		require.Equal(t, want, plainDecimal(in), in)
	}
}

// What Go writes must hash the same as what jsonb gives back.
func TestCanonicalSurvivesTheDatabase(t *testing.T) {
	written, err := canonicalJson(map[string]any{"confidence": 5e-7, "big": 1e21, "list": []any{1.5e-9}})
	require.NoError(t, err)
	stored, err := canonicalJson(json.RawMessage(`{"big": 1000000000000000000000, "confidence": 0.0000005, "list": [0.0000000015]}`))
	require.NoError(t, err)
	require.Equal(t, string(stored), string(written))
}

func TestCanonicalTreatsNegativeZeroAsZero(t *testing.T) {
	written, err := canonicalJson(map[string]any{"x": math.Copysign(0, -1)})
	require.NoError(t, err)
	stored, err := canonicalJson(json.RawMessage(`{"x":0}`))
	require.NoError(t, err)
	require.Equal(t, string(stored), string(written))
	for in, want := range map[string]string{"-0": "0", "-0.0": "0.0", "-0e5": "0", "-0.00E-1": "0.000", "-0.5": "-0.5"} {
		require.Equal(t, want, plainDecimal(in), in)
	}
}

func TestPlainDecimalKeepsScaleLikeNumeric(t *testing.T) {
	for in, want := range map[string]string{"1.50e1": "15.0", "1.500E+1": "15.00", "2.50e-1": "0.250", "1.5e1": "15"} {
		require.Equal(t, want, plainDecimal(in), in)
	}
}

// jsonb refuses \u0000, so a NUL anywhere would make the append fail.
func TestCanonicalReplacesNulInStringsAndKeys(t *testing.T) {
	got, err := canonicalJson(map[string]any{"text": "a\x00b", "nested": []any{map[string]any{"k\x00": "\x00"}}})
	require.NoError(t, err)
	require.NotContains(t, string(got), `\u0000`)
	require.Equal(t, `{"nested":[{"k`+"�"+`":"`+"�"+`"}],"text":"a`+"�"+`b"}`, string(got))
	again, err := canonicalJson(got)
	require.NoError(t, err)
	require.Equal(t, string(got), string(again), "canonical form is stable")
}

func TestNumbersPostgresCannotKeepAreNotSpelledOut(t *testing.T) {
	require.True(t, NumberFits("9007199254740993"))
	require.True(t, NumberFits("-0.12345678901234567891"))
	require.True(t, NumberFits("1e100"))
	require.True(t, NumberFits("1e-16383"))
	require.False(t, NumberFits("1e-16384"))
	require.False(t, NumberFits("1e-999999999"))
	require.False(t, NumberFits("1e999999999"))
	require.False(t, NumberFits("1e99999999999999999999"))

	// The encoder leaves such a number as written, in no time and memory.
	for _, number := range []string{"1e-999999999", "1e999999999", "-5e9223372036854775807"} {
		require.Equal(t, number, plainDecimal(number))
	}
	require.Equal(t, "15.0", plainDecimal("1.50e1"))
}

func TestNumberFitsSurvivesExtremeExponents(t *testing.T) {
	for _, number := range []string{"1e9223372036854775807", "1e-9223372036854775808", "-5e9223372036854775807", "1e9223372036854775808"} {
		require.False(t, NumberFits(number), number)
		require.Equal(t, number, plainDecimal(number))
	}
}

func TestPlainLengthBoundsTheCanonicalForm(t *testing.T) {
	// PlainLength is what a number costs to spell out: never less than the
	// canonical form, and not far above it.
	for _, number := range []string{"0", "15", "-15", "1.50e1", "1e3", "1e-3", "0.001", "123.456", "-9007199254740993", "1e-20", "12e-1", "5e2", "0.5e-2"} {
		spelled := len(plainDecimal(number))
		require.GreaterOrEqual(t, PlainLength(number), spelled-1, number)
		require.LessOrEqual(t, PlainLength(number), spelled+3, number)
	}
	require.Equal(t, -1, PlainLength("1e-999999999"))
	require.Greater(t, PlainLength("1e-16383"), 16000, "a short number can be long spelled out")
}
