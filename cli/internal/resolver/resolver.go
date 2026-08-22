package resolver

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/SheltonZhu/115driver/internal/remoteresolver"
)

const RootID = remoteresolver.RootID
const fileResolvePageLimit = remoteresolver.FileResolvePageLimit

type pathResolverClient = remoteresolver.Client
type PathResolver = remoteresolver.PathResolver

func New(client pathResolverClient) *PathResolver {
	return remoteresolver.New(client)
}

func newPathResolver(client pathResolverClient, capacity int) *PathResolver {
	return remoteresolver.NewWithCapacity(client, capacity)
}

func ResolveDir(client pathResolverClient, remotePath string) (string, error) {
	return remoteresolver.ResolveDir(client, remotePath)
}

func ResolveFile(client pathResolverClient, remotePath string) (string, error) {
	return remoteresolver.ResolveFile(client, remotePath)
}

func ResolvePath(client pathResolverClient, remotePath string) (string, bool, error) {
	return remoteresolver.ResolvePath(client, remotePath)
}

func ResolveLocalDownloadPath(localTarget, fileName string) string {
	if localTarget == "" {
		return fileName
	}

	if fi, err := osStat(localTarget); err == nil && fi.IsDir() {
		return filepath.Join(localTarget, fileName)
	}

	if strings.HasSuffix(localTarget, string(filepath.Separator)) {
		return filepath.Join(strings.TrimSuffix(localTarget, string(filepath.Separator)), fileName)
	}

	return localTarget
}

var osStat = func(name string) (os.FileInfo, error) {
	return os.Stat(name)
}
