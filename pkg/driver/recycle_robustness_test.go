package driver

import (
	"errors"
	"testing"
)

func TestValidateRecycleItemsRejectsMalformedSuccessfulEntries(t *testing.T) {
	for name, items := range map[string][]RecycleBinItem{
		"missing-id":      {{FileSize: 1}},
		"negative-size":   {{FileId: "r1", FileSize: -1}},
		"negative-delete": {{FileId: "r1", DeleteTime: -1}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateRecycleItems(items); !errors.Is(err, ErrUnexpected) {
				t.Fatalf("validateRecycleItems = %v, want ErrUnexpected", err)
			}
		})
	}
	if err := validateRecycleItems([]RecycleBinItem{{FileId: "r1", FileSize: 0, DeleteTime: 0}}); err != nil {
		t.Fatalf("valid recycle item rejected: %v", err)
	}
}
