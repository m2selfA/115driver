package transfer

import "testing"

func TestParseByteSize(t *testing.T) {
	tests := map[string]int64{
		"1":       1,
		"1B":      1,
		"32MiB":   32 << 20,
		"512 KiB": 512 << 10,
		"2MB":     2_000_000,
		"1GiB":    1 << 30,
	}
	for input, want := range tests {
		got, err := ParseByteSize(input)
		if err != nil {
			t.Fatalf("ParseByteSize(%q): %v", input, err)
		}
		if got != want {
			t.Fatalf("ParseByteSize(%q)=%d want %d", input, got, want)
		}
	}
	for _, input := range []string{"", "0", "-1", "1.5MiB", "12XB", "999999999999999999999999GiB"} {
		if _, err := ParseByteSize(input); err == nil {
			t.Fatalf("expected ParseByteSize(%q) to fail", input)
		}
	}
}
