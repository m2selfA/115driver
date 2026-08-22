package mcpapp

import (
	"github.com/SheltonZhu/115driver/internal/sessionconfig"
	syncjournalpkg "github.com/SheltonZhu/115driver/internal/syncjournal"
)

func newSyncJournalStore(config sessionconfig.Config, profileScope string) *syncjournalpkg.Store {
	return &syncjournalpkg.Store{
		Root: config.SessionDir, ProfileScope: profileScope,
		AutoGC: config.SessionAutoGC, GCInterval: config.SessionGCInterval,
		Retention: config.SessionRetention, TrashRetention: config.SessionTrashRetention,
	}
}
