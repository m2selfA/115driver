package driver

import (
	"errors"
	"io"
	"testing"
)

func TestScalarJSONTypesRejectEmptyInputWithoutPanicking(t *testing.T) {
	tests := map[string]func([]byte) error{
		"StringInt":     func(b []byte) error { var v StringInt; return v.UnmarshalJSON(b) },
		"StringInt64":   func(b []byte) error { var v StringInt64; return v.UnmarshalJSON(b) },
		"StringFloat64": func(b []byte) error { var v StringFloat64; return v.UnmarshalJSON(b) },
		"IntString":     func(b []byte) error { var v IntString; return v.UnmarshalJSON(b) },
		"BoolInt":       func(b []byte) error { var v BoolInt; return v.UnmarshalJSON(b) },
		"StringTime":    func(b []byte) error { var v StringTime; return v.UnmarshalJSON(b) },
		"DataString":    func(b []byte) error { var v DataString; return v.UnmarshalJSON(b) },
	}
	for name, unmarshal := range tests {
		t.Run(name, func(t *testing.T) {
			if err := unmarshal(nil); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("UnmarshalJSON(nil) = %v, want io.ErrUnexpectedEOF", err)
			}
			if err := unmarshal([]byte{}); !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("UnmarshalJSON(empty) = %v, want io.ErrUnexpectedEOF", err)
			}
		})
	}
}

func TestStringIntRejectsInvalidQuotedNumber(t *testing.T) {
	var value StringInt
	if err := value.UnmarshalJSON([]byte(`"not-an-int"`)); err == nil {
		t.Fatal("StringInt accepted invalid quoted integer")
	}
}
