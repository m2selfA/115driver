package syncjournal

import (
	"strings"
	"testing"
	"time"

	syncplanpkg "github.com/SheltonZhu/115driver/internal/syncplan"
)

func TestPostconditionEqualBindsOptionalEvidence(t *testing.T) {
	expected := &Postcondition{Side: "remote", Exists: true, Kind: "file", RemoteID: "id", Size: 7, SHA1: strings.Repeat("A", 40), ModTimeUnixNano: 9}
	actual := &Postcondition{Side: "remote", Exists: true, Kind: "file", RemoteID: "id", Size: 7, SHA1: strings.Repeat("a", 40), ModTimeUnixNano: 9}
	if !PostconditionEqual(expected, actual) {
		t.Fatal("equal completion evidence rejected")
	}
	actual.SHA1 = strings.Repeat("B", 40)
	if PostconditionEqual(expected, actual) {
		t.Fatal("changed digest accepted")
	}
	if !PostconditionEqual(&Postcondition{Side: "local", Exists: false}, &Postcondition{Side: "local", Exists: false}) {
		t.Fatal("matching absence evidence rejected")
	}
}

func TestCompareExpectedTrees(t *testing.T) {
	plan := syncplanpkg.Plan{Items: []syncplanpkg.Item{
		{RelativePath: "dir", Action: "skip", Kind: "directory", LocalPresent: true, RemotePresent: true, LocalPath: "/local/dir", RemotePath: "/remote/dir", RemoteID: "dir-id", LocalModTimeUnixNano: 10, RemoteModTimeUnixNano: 20},
		{RelativePath: "dir/file.bin", Action: "skip", Kind: "file", LocalPresent: true, RemotePresent: true, LocalPath: "/local/dir/file.bin", RemotePath: "/remote/dir/file.bin", RemoteID: "file-id", LocalSize: 7, RemoteSize: 7, RemoteSHA1: strings.Repeat("C", 40), LocalModTimeUnixNano: 11, RemoteModTimeUnixNano: 21},
	}}
	local := map[string]syncplanpkg.Entry{
		syncplanpkg.PathKey("dir"):          {RelativePath: "dir", Kind: "directory", ModTimeUnixNano: 10},
		syncplanpkg.PathKey("dir/file.bin"): {RelativePath: "dir/file.bin", Kind: "file", Size: 7, ModTimeUnixNano: 11},
	}
	remote := map[string]syncplanpkg.Entry{
		syncplanpkg.PathKey("dir"):          {RelativePath: "dir", Kind: "directory", RemoteID: "dir-id", ModTimeUnixNano: 20},
		syncplanpkg.PathKey("dir/file.bin"): {RelativePath: "dir/file.bin", Kind: "file", RemoteID: "file-id", Size: 7, SHA1: strings.Repeat("c", 40), ModTimeUnixNano: 21},
	}
	if err := CompareExpectedLocalTree(plan, local); err != nil {
		t.Fatal(err)
	}
	if err := CompareExpectedRemoteTree(plan, remote, nil); err != nil {
		t.Fatal(err)
	}
	remote[syncplanpkg.PathKey("dir/file.bin")] = syncplanpkg.Entry{RelativePath: "dir/file.bin", Kind: "file", RemoteID: "other", Size: 7, SHA1: strings.Repeat("c", 40), ModTimeUnixNano: 21}
	if err := CompareExpectedRemoteTree(plan, remote, nil); err == nil {
		t.Fatal("changed remote identity accepted")
	}
}

func TestRemoveInterruptedDownloadArtifacts(t *testing.T) {
	plan := projectionPlan(syncplanpkg.Item{RelativePath: "dir/file.bin", Action: "download", Kind: "file"})
	journal, err := New(plan, strings.Repeat("a", 64), 42, time.Unix(100, 0))
	if err != nil {
		t.Fatal(err)
	}
	journal.Items[0].State = "failed"
	journal.Items[0].Phase = PhaseMutationStarted
	current := map[string]syncplanpkg.Entry{
		syncplanpkg.PathKey("dir/.file.bin.115driver.part"):        {RelativePath: "dir/.file.bin.115driver.part", Kind: "file"},
		syncplanpkg.PathKey("dir/.file.bin.115driver.resume.json"): {RelativePath: "dir/.file.bin.115driver.resume.json", Kind: "file"},
		syncplanpkg.PathKey("dir/keep.bin"):                        {RelativePath: "dir/keep.bin", Kind: "file"},
	}
	RemoveInterruptedDownloadArtifacts(current, journal)
	if len(current) != 1 {
		t.Fatalf("download artifacts were not removed: %#v", current)
	}
	if _, ok := current[syncplanpkg.PathKey("dir/keep.bin")]; !ok {
		t.Fatal("unrelated local entry was removed")
	}
}

func TestCompareExpectedRemoteTreeUsesDigestResolver(t *testing.T) {
	plan := syncplanpkg.Plan{Items: []syncplanpkg.Item{{RelativePath: "file.bin", Action: "skip", Kind: "file", RemotePresent: true, RemoteID: "id", RemoteSize: 3, RemoteSHA1: strings.Repeat("D", 40)}}}
	current := map[string]syncplanpkg.Entry{syncplanpkg.PathKey("file.bin"): {RelativePath: "file.bin", Kind: "file", RemoteID: "id", Size: 3}}
	calls := 0
	if err := CompareExpectedRemoteTree(plan, current, func(entry syncplanpkg.Entry) (string, error) {
		calls++
		return strings.Repeat("d", 40), nil
	}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("digest resolver calls = %d, want 1", calls)
	}
}
