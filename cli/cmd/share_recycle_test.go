package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/spf13/cobra"
)

type fakeShareListClient struct {
	response *driver.ShareSnapResp
	err      error
	code     string
	receive  string
	dirID    string
}

func (f *fakeShareListClient) GetShareSnap(code, receive, dirID string, queries ...driver.Query) (*driver.ShareSnapResp, error) {
	f.code, f.receive, f.dirID = code, receive, dirID
	return f.response, f.err
}

type fakeRecycleListClient struct {
	pages    map[int][]driver.RecycleBinItem
	calls    int
	items    []driver.RecycleBinItem
	err      error
	offset   int
	limit    int
	reverted []string
}

func (f *fakeRecycleListClient) ListRecycleBin(offset, limit int) ([]driver.RecycleBinItem, error) {
	f.offset, f.limit = offset, limit
	f.calls++
	if f.pages != nil {
		if page, ok := f.pages[offset]; ok {
			return append([]driver.RecycleBinItem(nil), page...), f.err
		}
	}
	return f.items, f.err
}

func (f *fakeRecycleListClient) RevertRecycleBin(ids ...string) error {
	f.reverted = append([]string(nil), ids...)
	return f.err
}

func TestShareListArgsRejectsMissingSecretAndInvalidPagination(t *testing.T) {
	t.Setenv(envShareReceiveCode, "")
	oldReceive, oldDir, oldOffset, oldLimit := shareReceiveCode, shareDirID, shareOffset, shareLimit
	t.Cleanup(func() {
		shareReceiveCode, shareDirID, shareOffset, shareLimit = oldReceive, oldDir, oldOffset, oldLimit
	})

	for name, configure := range map[string]func(){
		"receive": func() { shareReceiveCode = "" },
		"dir":     func() { shareReceiveCode = "abcd"; shareDirID = "" },
		"offset":  func() { shareReceiveCode = "abcd"; shareDirID = "0"; shareOffset = -1 },
		"limit":   func() { shareReceiveCode = "abcd"; shareDirID = "0"; shareOffset = 0; shareLimit = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			shareReceiveCode, shareDirID, shareOffset, shareLimit = "abcd", "0", 0, defaultShareListLimit
			configure()
			err := shareListArgs(shareListCmd, []string{"share-code"})
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != output.ExitArgs {
				t.Fatalf("share args error = %T %v, want ExitArgs", err, err)
			}
		})
	}
}

func TestResolveShareReceiveCodeUsesEnvAndHonorsExplicitFlag(t *testing.T) {
	t.Setenv(envShareReceiveCode, "env-code")
	cmd := &cobra.Command{Use: "test"}
	cmd.Flags().String("receive-code", "", "test receive code")

	got, err := resolveShareReceiveCode(cmd, "")
	if err != nil || got != "env-code" {
		t.Fatalf("env receive code = %q err=%v, want env-code", got, err)
	}
	if err := cmd.Flags().Set("receive-code", "flag-code"); err != nil {
		t.Fatal(err)
	}
	got, err = resolveShareReceiveCode(cmd, "flag-code")
	if err != nil || got != "flag-code" {
		t.Fatalf("explicit receive code = %q err=%v, want flag-code", got, err)
	}
	if err := cmd.Flags().Set("receive-code", "   "); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveShareReceiveCode(cmd, "   "); err == nil {
		t.Fatal("explicit empty receive code unexpectedly fell back to environment")
	}

	t.Setenv(envShareReceiveCode, "")
	fresh := &cobra.Command{Use: "test"}
	fresh.Flags().String("receive-code", "", "test receive code")
	if _, err := resolveShareReceiveCode(fresh, ""); err == nil || !strings.Contains(err.Error(), envShareReceiveCode) {
		t.Fatalf("missing receive code error = %v, want environment hint", err)
	}
}

func TestLoadShareListBuildsStableResultWithoutReceiveCode(t *testing.T) {
	response := &driver.ShareSnapResp{}
	response.Data.Userinfo.UserName = "owner"
	response.Data.Shareinfo.ShareTitle = "dataset"
	response.Data.Shareinfo.ReceiveCode = "secret-code"
	response.Data.Count = 13
	response.Data.List = []driver.ShareFile{
		{FileID: "f1", FileName: "a.bin", Size: driver.StringInt64(12), IsFile: 1, Sha1: "ABC", ParentID: "0"},
		{FileID: "d1", FileName: "folder", IsFile: 0, ParentID: "0"},
	}
	client := &fakeShareListClient{response: response}
	result, err := loadShareList(client, "share-code", "secret-code", "0", 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	if client.code != "share-code" || client.receive != "secret-code" || client.dirID != "0" {
		t.Fatalf("unexpected share request: %#v", client)
	}
	if result.Returned != 2 || result.NextOffset != 12 || !result.HasMore || result.Items[0].Directory || !result.Items[1].Directory {
		t.Fatalf("unexpected share list result: %#v", result)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret-code") || strings.Contains(string(encoded), "receive_code") {
		t.Fatalf("receive code leaked into result JSON: %s", encoded)
	}
}

func TestRecycleListArgsStrictPagination(t *testing.T) {
	oldOffset, oldLimit := recycleOffset, recycleLimit
	t.Cleanup(func() { recycleOffset, recycleLimit = oldOffset, oldLimit })
	for name, values := range map[string][2]int{
		"negative-offset": {-1, 40},
		"zero-limit":      {0, 0},
		"too-large":       {0, 101},
	} {
		t.Run(name, func(t *testing.T) {
			recycleOffset, recycleLimit = values[0], values[1]
			err := recycleListArgs(recycleListCmd, nil)
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != output.ExitArgs {
				t.Fatalf("recycle args error = %T %v, want ExitArgs", err, err)
			}
		})
	}
}

func TestLoadRecycleListPreservesPaginationMetadata(t *testing.T) {
	client := &fakeRecycleListClient{items: []driver.RecycleBinItem{
		{FileId: "r1", FileName: "a.bin", FileSize: driver.StringInt64(10), ParentId: driver.IntString("0"), ParentName: "root", DeleteTime: driver.StringInt64(100)},
		{FileId: "r2", FileName: "b.bin", FileSize: driver.StringInt64(20), ParentId: driver.IntString("0"), ParentName: "root", DeleteTime: driver.StringInt64(200)},
	}}
	result, err := loadRecycleList(client, 4, 2)
	if err != nil {
		t.Fatal(err)
	}
	if client.offset != 4 || client.limit != 2 || result.Returned != 2 || result.NextOffset != 6 || !result.PageFull {
		t.Fatalf("unexpected recycle list result: %#v client=%#v", result, client)
	}
}

func TestFindRecycleItemsRejectsRepeatedFullPage(t *testing.T) {
	page := make([]driver.RecycleBinItem, maxRecycleListLimit)
	for i := range page {
		page[i] = driver.RecycleBinItem{FileId: fmt.Sprintf("r-%d", i)}
	}
	client := &fakeRecycleListClient{pages: map[int][]driver.RecycleBinItem{
		0:                   page,
		maxRecycleListLimit: page,
	}}
	items, missing, err := findRecycleItems(client, []string{"not-present"})
	if items != nil || missing != nil || !errors.Is(err, driver.ErrUnexpected) {
		t.Fatalf("repeated recycle page = %#v, %#v, %v; want ErrUnexpected", items, missing, err)
	}
	if client.calls != 2 {
		t.Fatalf("repeated recycle page calls = %d, want 2", client.calls)
	}
}

func TestRecycleRestoreDryRunValidatesWithoutMutation(t *testing.T) {
	oldDryRun, oldJSON, oldPrinter := recycleRestoreDryRun, jsonOutput, printer
	t.Cleanup(func() {
		recycleRestoreDryRun, jsonOutput, printer = oldDryRun, oldJSON, oldPrinter
	})
	recycleRestoreDryRun = true
	jsonOutput = true
	printer = output.NewPrinter(false)
	client := &fakeRecycleListClient{items: []driver.RecycleBinItem{
		{FileId: "r1", FileName: "a.bin", FileSize: driver.StringInt64(10)},
		{FileId: "r2", FileName: "b.bin", FileSize: driver.StringInt64(20)},
	}}
	cmd := newBatchInputTestCommand(t, "")
	if err := runRecycleRestore(client, cmd, []string{"r1", "r2", "r1"}); err != nil {
		t.Fatal(err)
	}
	if len(client.reverted) != 0 {
		t.Fatalf("dry-run restored IDs: %#v", client.reverted)
	}
}

func TestRecycleRestoreDryRunFailsClosedForMissingID(t *testing.T) {
	oldDryRun, oldJSON, oldPrinter := recycleRestoreDryRun, jsonOutput, printer
	t.Cleanup(func() {
		recycleRestoreDryRun, jsonOutput, printer = oldDryRun, oldJSON, oldPrinter
	})
	recycleRestoreDryRun = true
	jsonOutput = true
	printer = output.NewPrinter(false)
	client := &fakeRecycleListClient{items: []driver.RecycleBinItem{{FileId: "r1", FileName: "a.bin"}}}
	cmd := newBatchInputTestCommand(t, "")
	err := runRecycleRestore(client, cmd, []string{"r1", "missing"})
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != output.ExitNotFound || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("missing restore target error = %T %v, want ExitNotFound", err, err)
	}
	if len(client.reverted) != 0 {
		t.Fatalf("failed dry-run restored IDs: %#v", client.reverted)
	}
}

func TestRecycleRestoreUsesOneDeduplicatedBatchRequest(t *testing.T) {
	oldDryRun, oldJSON, oldPrinter := recycleRestoreDryRun, jsonOutput, printer
	t.Cleanup(func() {
		recycleRestoreDryRun, jsonOutput, printer = oldDryRun, oldJSON, oldPrinter
	})
	recycleRestoreDryRun = false
	jsonOutput = true
	printer = output.NewPrinter(false)
	client := &fakeRecycleListClient{}
	cmd := newBatchInputTestCommand(t, "")
	if err := runRecycleRestore(client, cmd, []string{" r1 ", "r2", "r1"}); err != nil {
		t.Fatal(err)
	}
	if len(client.reverted) != 2 || client.reverted[0] != "r1" || client.reverted[1] != "r2" {
		t.Fatalf("restore request IDs = %#v, want [r1 r2]", client.reverted)
	}
}

func TestRecycleRestoreCommandExposesBatchInputAndDryRun(t *testing.T) {
	if recycleRestoreCmd.Flags().Lookup("from-file") == nil || recycleRestoreCmd.Flags().Lookup("dry-run") == nil {
		t.Fatal("recycle restore is missing --from-file or --dry-run")
	}
	if err := recycleRestoreCmd.Args(recycleRestoreCmd, nil); err == nil {
		t.Fatal("recycle restore accepted no item IDs without --from-file")
	}
}
