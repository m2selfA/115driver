package cmd

import (
	"testing"

	"github.com/SheltonZhu/115driver/internal/transfer"
)

func TestFilterSessionEntriesCombinesStateDirectionKindAndFlags(t *testing.T) {
	entries := []transfer.SessionEntry{
		{
			ID: "stale-upload-file",
			Manifest: transfer.SessionManifest{
				State:    "active",
				Identity: transfer.SessionIdentityV2{Direction: "upload", Kind: "file"},
			},
			Stale: true,
		},
		{
			ID: "locked-download-tree",
			Manifest: transfer.SessionManifest{
				State:    "active",
				Identity: transfer.SessionIdentityV2{Direction: "download", Kind: "tree"},
			},
			InUse: true,
		},
		{
			ID: "completed-upload-tree",
			Manifest: transfer.SessionManifest{
				State:    "completed",
				Identity: transfer.SessionIdentityV2{Direction: "upload", Kind: "tree"},
			},
		},
		{ID: "corrupt", Corrupt: true},
		{ID: "newer", NewerVersion: true},
	}

	filtered, err := filterSessionEntries(entries, " ACTIVE ", "UPLOAD", "file", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].ID != "stale-upload-file" {
		t.Fatalf("unexpected combined filter result: %#v", filtered)
	}

	filtered, err = filterSessionEntries(entries, "", "download", "tree", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].ID != "locked-download-tree" {
		t.Fatalf("unexpected in-use filter result: %#v", filtered)
	}

	filtered, err = filterSessionEntries(entries, "corrupt", "", "", false, false)
	if err != nil || len(filtered) != 1 || filtered[0].ID != "corrupt" {
		t.Fatalf("unexpected corrupt filter result: %#v err=%v", filtered, err)
	}
	filtered, err = filterSessionEntries(entries, "newer", "", "", false, false)
	if err != nil || len(filtered) != 1 || filtered[0].ID != "newer" {
		t.Fatalf("unexpected newer filter result: %#v err=%v", filtered, err)
	}
}

func TestFilterSessionEntriesRejectsInvalidSelectors(t *testing.T) {
	tests := map[string][3]string{
		"state":     {"paused", "", ""},
		"direction": {"", "sideways", ""},
		"kind":      {"", "", "bundle"},
	}
	for name, selectors := range tests {
		t.Run(name, func(t *testing.T) {
			state, direction, kind := selectors[0], selectors[1], selectors[2]
			if _, err := filterSessionEntries(nil, state, direction, kind, false, false); err == nil {
				t.Fatalf("invalid selector was accepted: state=%q direction=%q kind=%q", state, direction, kind)
			}
		})
	}
}

func TestSessionsListExposesUsefulFilters(t *testing.T) {
	for _, name := range []string{"state", "direction", "kind", "stale", "in-use"} {
		if sessionsListCmd.Flags().Lookup(name) == nil {
			t.Fatalf("sessions list is missing --%s", name)
		}
	}
}
