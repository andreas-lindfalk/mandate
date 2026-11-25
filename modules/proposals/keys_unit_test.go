package proposals

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAmbiguousKey(t *testing.T) {
	for raw, want := range map[string]string{
		`{"amountCents": 100, "amountcents": 999999}`: "amountcents",
		`{"a": 1, "a": 2}`:                                 "a",
		`{"ſtatus": "x"}`:                                  "ſtatus",
		`{"Kind": 1}`:                                      "Kind",
		`{"outer": {"Id": 1, "ID": 2}}`:                    "ID",
		`{"list": [{"x": 1}, {"X": 2, "x": 3}]}`:           "x",
		`{"a": {"b": 1}, "A2": [1, {"c": [2, {"C": 3}]}]}`: "",
		`{"a": 1, "b": {"a": 2}, "c": [{"a": 3}]}`:         "",
		`{"name": "AMOUNTcents", "x": ["amountcents"]}`:    "",
	} {
		key, ambiguous := ambiguousKey(json.RawMessage(raw))
		require.Equal(t, want != "", ambiguous, raw)
		require.Equal(t, want, key, raw)
	}
}
