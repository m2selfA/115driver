package syncplan

import (
	"strings"
	"testing"
)

func TestBuildDownloadBindsRemoteContentSnapshotIntoFingerprint(t *testing.T) {
	remote := map[string]Entry{
		"remote.bin": testEntry("remote.bin", "file", 4, ""),
	}
	build := func(content string) Plan {
		t.Helper()
		calls := 0
		plan, err := Build(nil, remote, "/local", "/remote", "root", Options{
			Direction:      DirectionDownload,
			ConflictPolicy: ConflictError,
		}, Resolvers{
			RemoteSHA1: func(entry Entry) (string, error) {
				calls++
				return testSHA1(content), nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if calls != 1 || !plan.Ready || len(plan.Items) != 1 || plan.Items[0].Action != "download" || plan.Items[0].RemoteSHA1 != testSHA1(content) {
			t.Fatalf("download content snapshot plan=%#v resolver_calls=%d", plan, calls)
		}
		return plan
	}

	first := build("AAAA")
	second := build("BBBB")
	if first.PlanID == second.PlanID {
		t.Fatalf("remote download content change preserved plan id %q", first.PlanID)
	}

	if _, err := Build(nil, remote, "/local", "/remote", "root", Options{
		Direction:      DirectionDownload,
		ConflictPolicy: ConflictError,
	}, Resolvers{}); err == nil || !strings.Contains(err.Error(), "remote SHA1 resolver is required") {
		t.Fatalf("download without content resolver error = %v", err)
	}
}

func TestBuildDownloadUsesListingSHA1WithoutResolverCall(t *testing.T) {
	remote := map[string]Entry{
		"remote.bin": testEntry("remote.bin", "file", 4, testSHA1("AAAA")),
	}
	calls := 0
	plan, err := Build(nil, remote, "/local", "/remote", "root", Options{
		Direction:      DirectionDownload,
		ConflictPolicy: ConflictError,
	}, Resolvers{
		RemoteSHA1: func(entry Entry) (string, error) {
			calls++
			return testSHA1("UNEXPECTED"), nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || len(plan.Items) != 1 || plan.Items[0].RemoteSHA1 != testSHA1("AAAA") {
		t.Fatalf("listing SHA1 was not reused: plan=%#v resolver_calls=%d", plan, calls)
	}
}
