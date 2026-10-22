package jsonutil

import "testing"

func TestValidateJSONDistinctions(t *testing.T) {
	for _, raw := range []string{`{"a":[],"b":{"x":1}}`, `[]`, `42`, `null`, `{"a":null}`} {
		if err := Validate([]byte(raw), false); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{`null`, `{"a":[null]}`, `{"a":1,"a":2}`, `{"a":1,"\u0061":2}`, `{"a":{"b":1,"b":2}}`, `{} []`, `[}`, `{"a":}`, ``} {
		if err := Validate([]byte(raw), true); err == nil {
			t.Errorf("accepted %s", raw)
		}
	}
	if err := Validate([]byte(`{"a":1,"a":2}`), false); err == nil {
		t.Fatal("duplicates accepted when null allowed")
	}
}
