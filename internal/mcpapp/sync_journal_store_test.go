package mcpapp

import (
	"strings"
	"testing"
	"time"

	"github.com/SheltonZhu/115driver/internal/sessionconfig"
)

func TestNewSyncJournalStorePropagatesSessionMaintenanceConfig(t *testing.T) {
	config := sessionconfig.Config{
		SessionDir:            t.TempDir(),
		SessionAutoGC:         true,
		SessionGCInterval:     17 * time.Minute,
		SessionRetention:      11 * time.Hour,
		SessionTrashRetention: 23 * time.Hour,
	}
	scope := strings.Repeat("a", 64)
	store := newSyncJournalStore(config, scope)
	if store == nil || store.Root != config.SessionDir || store.ProfileScope != scope || !store.AutoGC || store.GCInterval != config.SessionGCInterval || store.Retention != config.SessionRetention || store.TrashRetention != config.SessionTrashRetention {
		t.Fatalf("sync journal store did not preserve session maintenance config: %#v", store)
	}
}
