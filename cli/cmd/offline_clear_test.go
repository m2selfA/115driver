package cmd

import (
	"errors"
	"net"
	"testing"

	"github.com/SheltonZhu/115driver/cli/internal/output"
	"github.com/SheltonZhu/115driver/pkg/driver"
)

type fakeOfflineClearClient struct {
	tasks      []*driver.OfflineTask
	listErr    error
	clearErr   error
	clearFlags []int64
}

func (f *fakeOfflineClearClient) ListOfflineTask(page int64) (driver.OfflineTaskResp, error) {
	if f.listErr != nil {
		return driver.OfflineTaskResp{}, f.listErr
	}
	if page != 1 {
		return driver.OfflineTaskResp{Page: page, PageCount: 1}, nil
	}
	return driver.OfflineTaskResp{Page: 1, PageCount: 1, Total: int64(len(f.tasks)), Tasks: f.tasks}, nil
}

func (f *fakeOfflineClearClient) ClearOfflineTasks(clearFlag int64) error {
	f.clearFlags = append(f.clearFlags, clearFlag)
	return f.clearErr
}

func preserveOfflineClearGlobals(t *testing.T) {
	t.Helper()
	oldScope, oldDryRun, oldForce := offlineClearScope, offlineClearDryRun, offlineClearForce
	oldJSON, oldPrinter := jsonOutput, printer
	t.Cleanup(func() {
		offlineClearScope, offlineClearDryRun, offlineClearForce = oldScope, oldDryRun, oldForce
		jsonOutput, printer = oldJSON, oldPrinter
	})
}

func TestOfflineClearArgsRequireForceAndStrictScope(t *testing.T) {
	preserveOfflineClearGlobals(t)
	for name, configure := range map[string]func(){
		"missing-force": func() { offlineClearScope, offlineClearDryRun, offlineClearForce = "completed", false, false },
		"invalid-scope": func() { offlineClearScope, offlineClearDryRun, offlineClearForce = "sideways", false, true },
	} {
		t.Run(name, func(t *testing.T) {
			configure()
			err := offlineClearArgs(offlineClearCmd, nil)
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != output.ExitArgs {
				t.Fatalf("offline clear args error = %T %v, want ExitArgs", err, err)
			}
		})
	}

	offlineClearScope, offlineClearDryRun, offlineClearForce = "all", true, false
	if err := offlineClearArgs(offlineClearCmd, nil); err != nil {
		t.Fatalf("dry-run without force rejected: %v", err)
	}
	offlineClearScope, offlineClearDryRun, offlineClearForce = "completed", false, true
	if err := offlineClearArgs(offlineClearCmd, nil); err != nil {
		t.Fatalf("forced clear rejected: %v", err)
	}
}

func TestOfflineClearClassifiesListAndClearNetworkFailures(t *testing.T) {
	for name, client := range map[string]*fakeOfflineClearClient{
		"list":  {listErr: &net.DNSError{Err: "offline", Name: "115.com", IsTemporary: true}},
		"clear": {tasks: []*driver.OfflineTask{{InfoHash: "done", Status: 2}}, clearErr: &net.DNSError{Err: "offline", Name: "115.com", IsTemporary: true}},
	} {
		t.Run(name, func(t *testing.T) {
			preserveOfflineClearGlobals(t)
			offlineClearScope, offlineClearDryRun, offlineClearForce = "completed", false, true
			err := runOfflineClear(client)
			var ee *exitError
			if !errors.As(err, &ee) || ee.code != output.ExitNetwork {
				t.Fatalf("offline clear network error = %T %v, want ExitNetwork", err, err)
			}
		})
	}
}

func TestBuildOfflineClearPlanFiltersDocumentedScopes(t *testing.T) {
	client := &fakeOfflineClearClient{tasks: []*driver.OfflineTask{
		{InfoHash: "done", Name: "done-task", Status: 2, Percent: 1, Size: 10},
		{InfoHash: "queued", Name: "queued-task", Status: 0, Percent: 0, Size: 15},
		{InfoHash: "running", Name: "running-task", Status: 1, Percent: 0.5, Size: 20},
		{InfoHash: "failed", Name: "failed-task", Status: -1, Percent: 0.2, Size: 30},
	}}
	for name, tc := range map[string]struct {
		scope string
		want  []string
	}{
		"completed": {scope: "completed", want: []string{"done"}},
		"failed":    {scope: "failed", want: []string{"failed"}},
		"active":    {scope: "active", want: []string{"queued", "running"}},
		"all":       {scope: "all", want: []string{"done", "queued", "running", "failed"}},
	} {
		t.Run(name, func(t *testing.T) {
			plan, err := buildOfflineClearPlan(client, tc.scope)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Total != 4 || plan.Matched != len(tc.want) || len(plan.Items) != len(tc.want) {
				t.Fatalf("unexpected %s plan counts: %#v", tc.scope, plan)
			}
			for i, want := range tc.want {
				if plan.Items[i].Hash != want {
					t.Fatalf("%s plan item %d = %q, want %q", tc.scope, i, plan.Items[i].Hash, want)
				}
			}
		})
	}
}

func TestRunOfflineClearDryRunNeverMutates(t *testing.T) {
	preserveOfflineClearGlobals(t)
	offlineClearScope, offlineClearDryRun, offlineClearForce = "all", true, false
	jsonOutput = true
	printer = output.NewPrinter(false)
	client := &fakeOfflineClearClient{tasks: []*driver.OfflineTask{{InfoHash: "one", Status: 1}}}
	if err := runOfflineClear(client); err != nil {
		t.Fatal(err)
	}
	if len(client.clearFlags) != 0 {
		t.Fatalf("dry-run submitted clear flags: %#v", client.clearFlags)
	}
}

func TestRunOfflineClearUsesDocumentedDriverFlags(t *testing.T) {
	for name, tc := range map[string]struct {
		scope string
		flag  int64
		state int
	}{
		"completed": {scope: "completed", flag: 0, state: 2},
		"all":       {scope: "all", flag: 1, state: 1},
		"failed":    {scope: "failed", flag: 2, state: -1},
		"active":    {scope: "active", flag: 3, state: 1},
	} {
		t.Run(name, func(t *testing.T) {
			preserveOfflineClearGlobals(t)
			offlineClearScope, offlineClearDryRun, offlineClearForce = tc.scope, false, true
			jsonOutput = true
			printer = output.NewPrinter(false)
			client := &fakeOfflineClearClient{tasks: []*driver.OfflineTask{{InfoHash: "one", Status: tc.state}}}
			if err := runOfflineClear(client); err != nil {
				t.Fatal(err)
			}
			if len(client.clearFlags) != 1 || client.clearFlags[0] != tc.flag {
				t.Fatalf("clear flags = %#v, want [%d]", client.clearFlags, tc.flag)
			}
		})
	}
}
