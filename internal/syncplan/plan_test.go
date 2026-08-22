package syncplan

import (
	"crypto/sha1"
	"encoding/hex"
	"strings"
	"testing"

	uploadpkg "github.com/SheltonZhu/115driver/internal/upload"
)

func testSHA1(value string) string {
	digest := sha1.Sum([]byte(value))
	return strings.ToUpper(hex.EncodeToString(digest[:]))
}

func testEntry(relative, kind string, size int64, sha string) Entry {
	return Entry{RelativePath: relative, Kind: kind, LocalPath: "/local/" + relative, RemotePath: "/remote/" + relative, RemoteID: "id-" + relative, Size: size, SHA1: sha}
}

func TestBuildPreferLocalUnavailableRemoteSHA1RemainsConflict(t *testing.T) {
	local := map[string]Entry{"unknown.bin": testEntry("unknown.bin", "file", 3, "")}
	remote := map[string]Entry{"unknown.bin": testEntry("unknown.bin", "file", 3, "")}
	remoteCalls := 0
	plan, err := Build(local, remote, "/local", "/remote", "root", Options{Direction: DirectionUpload, ConflictPolicy: ConflictLocal}, Resolvers{
		RemoteSHA1: func(Entry) (string, error) {
			remoteCalls++
			return "", nil
		},
		LocalDigest: func(entry Entry) (*uploadpkg.PreparedDigest, error) {
			return &uploadpkg.PreparedDigest{SHA1: testSHA1("abc"), Size: entry.Size, ModTimeUnixNano: entry.ModTimeUnixNano}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 {
		t.Fatalf("unexpected plan items: %#v", plan.Items)
	}
	item := plan.Items[0]
	if plan.Ready || item.Action != "conflict" || item.Reason != "remote-sha1-unavailable" || item.Destructive || item.LocalSHA1 != "" || item.RemoteSHA1 != "" || item.LocalPreparedDigest != nil || plan.ChecksummedFiles != 0 || plan.ChecksummedBytes != 0 || plan.Conflicts != 1 || plan.ResolvedConflicts != 0 || remoteCalls != 1 {
		t.Fatalf("unavailable remote SHA1 prefer-local plan=%#v item=%#v remote_calls=%d", plan, item, remoteCalls)
	}
}

func TestPlanDeleteTotalsIncludesBothSidesAndCoveredSubtrees(t *testing.T) {
	plan := Plan{
		DeleteRemoteRoots: 2, DeleteRemoteFiles: 3, DeleteRemoteDirs: 4, DeleteRemoteBytes: 11,
		DeleteLocalRoots: 5, DeleteLocalFiles: 6, DeleteLocalDirs: 7, DeleteLocalBytes: 13,
	}
	roots, items, bytes := plan.DeleteTotals()
	if roots != 7 || items != 20 || bytes != 24 {
		t.Fatalf("delete totals = roots=%d items=%d bytes=%d, want 7/20/24", roots, items, bytes)
	}
}

func TestBuildReplacementDeleteTotalsIncludeCoveredOldSubtrees(t *testing.T) {
	t.Run("replace-remote-old-directory", func(t *testing.T) {
		local := map[string]Entry{
			"node": testEntry("node", "file", 4, ""),
		}
		remote := map[string]Entry{
			"node":             testEntry("node", "directory", 0, ""),
			"node/sub":         testEntry("node/sub", "directory", 0, ""),
			"node/sub/old.bin": testEntry("node/sub/old.bin", "file", 7, testSHA1("old")),
		}
		plan, err := Build(local, remote, "/local", "/remote", "root", Options{Direction: DirectionUpload, ConflictPolicy: ConflictLocal}, Resolvers{
			LocalDigest: func(entry Entry) (*uploadpkg.PreparedDigest, error) {
				return &uploadpkg.PreparedDigest{SHA1: testSHA1("winner"), Size: entry.Size}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		roots, items, bytes := plan.DeleteTotals()
		if roots != 1 || items != 3 || bytes != 7 || plan.DeleteRemoteRoots != 1 || plan.DeleteRemoteDirs != 2 || plan.DeleteRemoteFiles != 1 || plan.DeleteRemoteBytes != 7 || plan.DeleteLocalRoots != 0 {
			t.Fatalf("replace-remote delete impact = plan=%#v totals=%d/%d/%d", plan, roots, items, bytes)
		}
		if plan.CoveredByDelete != 0 || plan.Items[1].Action != "skip" || plan.Items[2].Action != "skip" {
			t.Fatalf("replace-remote coverage semantics changed: %#v", plan.Items)
		}
	})

	t.Run("replace-local-old-directory", func(t *testing.T) {
		local := map[string]Entry{
			"node":             testEntry("node", "directory", 0, ""),
			"node/sub":         testEntry("node/sub", "directory", 0, ""),
			"node/sub/old.bin": testEntry("node/sub/old.bin", "file", 9, ""),
		}
		remote := map[string]Entry{
			"node": testEntry("node", "file", 5, testSHA1("winner")),
		}
		plan, err := Build(local, remote, "/local", "/remote", "root", Options{Direction: DirectionDownload, ConflictPolicy: ConflictRemote}, Resolvers{
			LocalDigest: func(entry Entry) (*uploadpkg.PreparedDigest, error) {
				return &uploadpkg.PreparedDigest{SHA1: testSHA1("old"), Size: entry.Size}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		roots, items, bytes := plan.DeleteTotals()
		if roots != 1 || items != 3 || bytes != 9 || plan.DeleteLocalRoots != 1 || plan.DeleteLocalDirs != 2 || plan.DeleteLocalFiles != 1 || plan.DeleteLocalBytes != 9 || plan.DeleteRemoteRoots != 0 {
			t.Fatalf("replace-local delete impact = plan=%#v totals=%d/%d/%d", plan, roots, items, bytes)
		}
		if plan.CoveredByDelete != 0 || plan.Items[1].Action != "skip" || plan.Items[2].Action != "skip" {
			t.Fatalf("replace-local coverage semantics changed: %#v", plan.Items)
		}
	})
}

func TestBuildConservativeClassificationAndFingerprint(t *testing.T) {
	local := map[string]Entry{}
	remote := map[string]Entry{}
	for _, entry := range []Entry{
		testEntry("same.bin", "file", 4, ""),
		testEntry("local.bin", "file", 2, ""),
		testEntry("dir", "directory", 0, ""),
	} {
		if err := AddEntry(local, entry, "local"); err != nil {
			t.Fatal(err)
		}
	}
	for _, entry := range []Entry{
		testEntry("same.bin", "file", 4, testSHA1("same")),
		testEntry("remote.bin", "file", 3, testSHA1("rem")),
		testEntry("dir", "directory", 0, ""),
	} {
		if err := AddEntry(remote, entry, "remote"); err != nil {
			t.Fatal(err)
		}
	}
	plan, err := Build(local, remote, "/local", "/remote", "root", Options{}, Resolvers{
		LocalDigest: func(entry Entry) (*uploadpkg.PreparedDigest, error) {
			return &uploadpkg.PreparedDigest{SHA1: testSHA1("same"), Size: entry.Size}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Ready || plan.ChangeActions != 2 || plan.UploadFiles != 1 || plan.DownloadFiles != 1 || plan.SkippedFiles != 1 || plan.SkippedDirs != 1 {
		t.Fatalf("unexpected plan summary: %#v", plan)
	}
	byPath := map[string]Item{}
	for _, item := range plan.Items {
		byPath[item.RelativePath] = item
	}
	if byPath["same.bin"].Action != "skip" || byPath["local.bin"].Action != "upload" || byPath["remote.bin"].Action != "download" {
		t.Fatalf("unexpected actions: %#v", byPath)
	}
	if len(plan.PlanID) != 64 || Fingerprint(plan) != plan.PlanID {
		t.Fatalf("unexpected fingerprint: %q", plan.PlanID)
	}
}

func TestBuildConflictPoliciesAndMirrorDelete(t *testing.T) {
	local := map[string]Entry{}
	remote := map[string]Entry{}
	_ = AddEntry(local, testEntry("node", "file", 4, ""), "local")
	_ = AddEntry(remote, testEntry("node", "directory", 0, ""), "remote")
	plan, err := Build(local, remote, "/local", "/remote", "root", Options{Direction: DirectionUpload, ConflictPolicy: ConflictLocal}, Resolvers{
		LocalDigest: func(entry Entry) (*uploadpkg.PreparedDigest, error) {
			return &uploadpkg.PreparedDigest{SHA1: testSHA1("node"), Size: entry.Size}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Ready || plan.DestructiveActions != 1 || plan.Items[0].Action != "replace-remote" || !plan.Items[0].Destructive {
		t.Fatalf("prefer-local type replacement = %#v", plan)
	}

	local = map[string]Entry{}
	remote = map[string]Entry{}
	_ = AddEntry(remote, testEntry("orphan", "directory", 0, ""), "remote")
	_ = AddEntry(remote, testEntry("orphan/file.bin", "file", 8, testSHA1("orphan")), "remote")
	plan, err = Build(local, remote, "/local", "/remote", "root", Options{Direction: DirectionUpload, ConflictPolicy: ConflictError, DeleteExtraneous: true}, Resolvers{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.DeleteRemoteRoots != 1 || plan.DeleteRemoteDirs != 1 || plan.DeleteRemoteFiles != 1 || plan.DeleteRemoteBytes != 8 || plan.CoveredByDelete != 1 {
		t.Fatalf("mirror delete accounting = %#v", plan)
	}
}

func TestBuildDestructiveRemoteFilesBindContentSnapshotsIntoFingerprint(t *testing.T) {
	tests := []struct {
		name       string
		local      map[string]Entry
		remote     map[string]Entry
		options    Options
		wantAction string
	}{
		{
			name:       "delete-remote-file",
			local:      map[string]Entry{},
			remote:     map[string]Entry{"orphan.bin": testEntry("orphan.bin", "file", 4, "")},
			options:    Options{Direction: DirectionUpload, ConflictPolicy: ConflictError, DeleteExtraneous: true},
			wantAction: "delete-remote",
		},
		{
			name:       "replace-remote-old-file",
			local:      map[string]Entry{"node": testEntry("node", "directory", 0, "")},
			remote:     map[string]Entry{"node": testEntry("node", "file", 4, "")},
			options:    Options{Direction: DirectionUpload, ConflictPolicy: ConflictLocal},
			wantAction: "replace-remote",
		},
		{
			name:       "replace-local-remote-winner",
			local:      map[string]Entry{"node": testEntry("node", "directory", 0, "")},
			remote:     map[string]Entry{"node": testEntry("node", "file", 4, "")},
			options:    Options{Direction: DirectionDownload, ConflictPolicy: ConflictRemote},
			wantAction: "replace-local",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			build := func(content string) Plan {
				t.Helper()
				calls := 0
				plan, err := Build(tt.local, tt.remote, "/local", "/remote", "root", tt.options, Resolvers{
					RemoteSHA1: func(entry Entry) (string, error) {
						calls++
						return testSHA1(content), nil
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				if calls != 1 || len(plan.Items) != 1 || plan.Items[0].Action != tt.wantAction || plan.Items[0].RemoteSHA1 != testSHA1(content) {
					t.Fatalf("destructive remote snapshot plan=%#v resolver_calls=%d", plan, calls)
				}
				return plan
			}
			first := build("AAAA")
			second := build("BBBB")
			if first.PlanID == second.PlanID {
				t.Fatalf("remote content change preserved destructive plan id %q", first.PlanID)
			}
			if _, err := Build(tt.local, tt.remote, "/local", "/remote", "root", tt.options, Resolvers{}); err == nil || !strings.Contains(err.Error(), "remote SHA1 resolver is required") {
				t.Fatalf("destructive remote plan without digest resolver error = %v", err)
			}
		})
	}
}

func TestBuildLocalOnlyUploadBindsContentDigestIntoFingerprint(t *testing.T) {
	local := map[string]Entry{}
	remote := map[string]Entry{}
	entry := testEntry("local.bin", "file", 4, "")
	entry.ModTimeUnixNano = 12345
	if err := AddEntry(local, entry, "local"); err != nil {
		t.Fatal(err)
	}
	build := func(content string) Plan {
		t.Helper()
		plan, err := Build(local, remote, "/local", "/remote", "root", Options{Direction: DirectionUpload}, Resolvers{
			LocalDigest: func(entry Entry) (*uploadpkg.PreparedDigest, error) {
				return &uploadpkg.PreparedDigest{SHA1: testSHA1(content), Size: entry.Size, ModTimeUnixNano: entry.ModTimeUnixNano}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return plan
	}
	first := build("AAAA")
	second := build("BBBB")
	if first.PlanID == second.PlanID {
		t.Fatalf("local-only content change preserved plan id %q", first.PlanID)
	}
	if first.ChecksummedFiles != 1 || first.ChecksummedBytes != 4 || first.Items[0].LocalSHA1 == "" || first.Items[0].LocalPreparedDigest == nil {
		t.Fatalf("local-only upload did not bind content snapshot: %#v", first)
	}
	if _, err := Build(local, remote, "/local", "/remote", "root", Options{Direction: DirectionUpload}, Resolvers{}); err == nil || !strings.Contains(err.Error(), "local digest resolver is required") {
		t.Fatalf("local-only upload without digest resolver error = %v", err)
	}
}

func TestResolveOptionsAndPathContracts(t *testing.T) {
	if _, err := ResolveOptionsWithDelete(DirectionBoth, ConflictError, true); err == nil {
		t.Fatal("two-way delete accepted")
	}
	if _, err := ResolveOptions(DirectionUpload, ConflictRemote); err == nil {
		t.Fatal("incompatible policy accepted")
	}
	if got := CanonicalRemoteRoot("a/b/"); got != "/a/b" {
		t.Fatalf("canonical root = %q", got)
	}
	if got := RemoteChildPath("/a", "b/c"); got != "/a/b/c" {
		t.Fatalf("child = %q", got)
	}
	if err := ValidateRelativePath("a/../b"); err == nil {
		t.Fatal("relative traversal accepted")
	}
	entries := map[string]Entry{}
	if err := AddEntry(entries, testEntry("a.bin", "file", 1, ""), "local"); err != nil {
		t.Fatal(err)
	}
	if err := AddEntry(entries, testEntry("a.bin", "file", 1, ""), "local"); err == nil {
		t.Fatal("duplicate entry accepted")
	}
}
