package cmd

import "testing"

func TestSessionsListArgsRejectInvalidFiltersBeforeStoreAccess(t *testing.T) {
	oldState, oldDirection, oldKind := sessionsListState, sessionsListDirection, sessionsListKind
	t.Cleanup(func() {
		sessionsListState, sessionsListDirection, sessionsListKind = oldState, oldDirection, oldKind
	})

	for name, configure := range map[string]func(){
		"state":     func() { sessionsListState = "unknown" },
		"direction": func() { sessionsListDirection = "sideways" },
		"kind":      func() { sessionsListKind = "bundle" },
	} {
		t.Run(name, func(t *testing.T) {
			sessionsListState, sessionsListDirection, sessionsListKind = "", "", ""
			configure()
			requireExitArgs(t, sessionsListArgs(sessionsListCmd, nil))
		})
	}
}

func TestSessionsGCArgsRejectInvalidAgeBeforeStoreAccess(t *testing.T) {
	old := sessionsGCOlderThan
	t.Cleanup(func() { sessionsGCOlderThan = old })
	sessionsGCOlderThan = "not-an-age"
	requireExitArgs(t, sessionsGCArgs(sessionsGCCmd, nil))
}

func TestSyncJournalListArgsRejectInvalidStateBeforeStoreAccess(t *testing.T) {
	old := syncJournalListState
	t.Cleanup(func() { syncJournalListState = old })
	syncJournalListState = "unknown"
	requireExitArgs(t, syncJournalListArgs(syncJournalListCmd, nil))
}

func TestSyncJournalGCArgsRejectInvalidAgeBeforeStoreAccess(t *testing.T) {
	old := syncJournalGCOlderThan
	t.Cleanup(func() { syncJournalGCOlderThan = old })
	syncJournalGCOlderThan = "not-an-age"
	requireExitArgs(t, syncJournalGCArgs(syncJournalGCCmd, nil))
}

func TestLocalAdminArgsAcceptValidFiltersAndAges(t *testing.T) {
	oldSessionState, oldSessionDirection, oldSessionKind := sessionsListState, sessionsListDirection, sessionsListKind
	oldSessionAge := sessionsGCOlderThan
	oldJournalState, oldJournalAge := syncJournalListState, syncJournalGCOlderThan
	t.Cleanup(func() {
		sessionsListState, sessionsListDirection, sessionsListKind = oldSessionState, oldSessionDirection, oldSessionKind
		sessionsGCOlderThan = oldSessionAge
		syncJournalListState, syncJournalGCOlderThan = oldJournalState, oldJournalAge
	})

	sessionsListState, sessionsListDirection, sessionsListKind = "active", "upload", "tree"
	sessionsGCOlderThan = "30d"
	syncJournalListState = syncJournalStatusRecoveryRequired
	syncJournalGCOlderThan = "72h"

	for name, validate := range map[string]func() error{
		"sessions list": func() error { return sessionsListArgs(sessionsListCmd, nil) },
		"sessions gc":   func() error { return sessionsGCArgs(sessionsGCCmd, nil) },
		"journal list":  func() error { return syncJournalListArgs(syncJournalListCmd, nil) },
		"journal gc":    func() error { return syncJournalGCArgs(syncJournalGCCmd, nil) },
	} {
		if err := validate(); err != nil {
			t.Fatalf("%s rejected valid arguments: %v", name, err)
		}
	}
}
