package transfer

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode"
)

// DefaultChunkSize is the default byte range size used by chunk strategy.
const DefaultChunkSize int64 = 32 << 20 // 32 MiB

// ParseByteSize parses an integer byte size such as 33554432, 32MiB, 32MB,
// 512KiB, or 1GiB. IEC suffixes use powers of 1024; SI suffixes use powers of
// 1000. A suffix-less value is interpreted as bytes.
func ParseByteSize(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("byte size is empty")
	}
	cut := len(value)
	for i, r := range value {
		if !unicode.IsDigit(r) {
			cut = i
			break
		}
	}
	if cut == 0 {
		return 0, fmt.Errorf("invalid byte size %q", value)
	}
	numberText := strings.TrimSpace(value[:cut])
	suffix := strings.ToLower(strings.TrimSpace(value[cut:]))
	number, err := strconv.ParseUint(numberText, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid byte size %q: %w", value, err)
	}
	multipliers := map[string]uint64{
		"": 1, "b": 1,
		"kb": 1000, "mb": 1000 * 1000, "gb": 1000 * 1000 * 1000, "tb": 1000 * 1000 * 1000 * 1000,
		"kib": 1 << 10, "mib": 1 << 20, "gib": 1 << 30, "tib": 1 << 40,
	}
	multiplier, ok := multipliers[suffix]
	if !ok {
		return 0, fmt.Errorf("invalid byte size suffix %q", suffix)
	}
	if number > uint64(math.MaxInt64)/multiplier {
		return 0, fmt.Errorf("byte size %q overflows int64", value)
	}
	result := int64(number * multiplier)
	if result <= 0 {
		return 0, errors.New("byte size must be > 0")
	}
	return result, nil
}
