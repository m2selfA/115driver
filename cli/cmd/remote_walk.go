package cmd

import (
	"github.com/SheltonZhu/115driver/internal/remotetree"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

type remoteTreeListClient = remotetree.Client

type remoteWalkEntry = remotetree.Entry
type remoteWalkResult = remotetree.Result

func listRemoteDirectoryReadOnly(client remoteTreeListClient, dirID string) (*[]driver.File, error) {
	return remotetree.ListDirectoryReadOnly(client, dirID)
}

func validateRemoteTreeEntryName(name string) error {
	return remotetree.ValidateEntryName(name)
}

func walkRemoteTree(client remoteTreeListClient, rootID, rootPath string, maxDepth int, visit func(remoteWalkEntry) (bool, error)) (remoteWalkResult, error) {
	return remotetree.Walk(client, rootID, rootPath, maxDepth, visit)
}

func joinRemoteDisplayPath(parent, name string) string {
	return remotetree.JoinDisplayPath(parent, name)
}
