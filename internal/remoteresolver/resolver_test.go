package remoteresolver

import (
	"errors"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
)

type resolverContractClient struct {
	dirCalls  int
	listCalls int
	list      *[]driver.File
}

func (c *resolverContractClient) DirName2CID(dir string) (*driver.APIGetDirIDResp, error) {
	c.dirCalls++
	return &driver.APIGetDirIDResp{CategoryID: driver.IntString("0")}, nil
}

func (c *resolverContractClient) ListPage(dirID string, offset, limit int64, opts ...driver.ListOption) (*[]driver.File, error) {
	c.listCalls++
	return c.list, nil
}

func TestResolvePathCanonicalizesSlashOnlyRootWithoutNetwork(t *testing.T) {
	client := &resolverContractClient{}
	id, isDir, err := ResolvePath(client, "////")
	if err != nil || id != RootID || !isDir {
		t.Fatalf("ResolvePath(slash root) = id=%q dir=%v err=%v", id, isDir, err)
	}
	if client.dirCalls != 0 || client.listCalls != 0 {
		t.Fatalf("slash-only root reached network: dir=%d list=%d", client.dirCalls, client.listCalls)
	}
}

func TestResolveFileRejectsNilListingAsUnexpected(t *testing.T) {
	client := &resolverContractClient{list: nil}
	_, err := ResolveFile(client, "missing.bin")
	if !errors.Is(err, driver.ErrUnexpected) {
		t.Fatalf("ResolveFile nil listing = %v, want ErrUnexpected", err)
	}
	if client.listCalls != 1 {
		t.Fatalf("ResolveFile nil listing calls=%d, want 1", client.listCalls)
	}
}
