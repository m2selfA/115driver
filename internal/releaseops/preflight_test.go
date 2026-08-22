package releaseops

import (
	"strings"
	"testing"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

func TestEvaluateAbsentStableRelease(t *testing.T) {
	plan, err := Evaluate(Input{Tag: "v1.2.3", ExpectedSHA: testSHA, TagSHA: testSHA})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if plan.State != StateAbsent || plan.Prerelease || plan.ReadOnly {
		t.Fatalf("unexpected absent plan: %+v", plan)
	}
	if len(plan.ExpectedAssets) != 13 || len(plan.MissingAssets) != 13 {
		t.Fatalf("unexpected asset counts: expected=%d missing=%d", len(plan.ExpectedAssets), len(plan.MissingAssets))
	}
	if got := strings.Join(plan.Operations, ","); got != "build-candidate,attest-candidate,create-draft,verify-remote-roundtrip,publish" {
		t.Fatalf("unexpected operations: %s", got)
	}
}

func TestEvaluateAbsentRCIsPrerelease(t *testing.T) {
	plan, err := Evaluate(Input{Tag: "v1.2.3-rc.1", ExpectedSHA: testSHA, TagSHA: testSHA})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !plan.Prerelease {
		t.Fatal("RC tag must be classified as prerelease")
	}
}

func TestEvaluatePartialDraftIsRepairable(t *testing.T) {
	plan, err := Simulate(StateDraft, "115driver", "v1.2.3-rc.1", testSHA)
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	if plan.State != StateDraft || plan.ReadOnly || !plan.Prerelease || !plan.PrereleaseNeedsUpdate {
		t.Fatalf("unexpected draft plan: %+v", plan)
	}
	if len(plan.MissingAssets) == 0 {
		t.Fatal("partial draft simulation must have missing assets")
	}
	if got := strings.Join(plan.Operations, ","); got != "build-candidate,attest-candidate,repair-draft,verify-remote-roundtrip,publish" {
		t.Fatalf("unexpected operations: %s", got)
	}
}

func TestEvaluateDraftRejectsUnknownAsset(t *testing.T) {
	_, err := Evaluate(Input{
		Tag:         "v1.2.3",
		ExpectedSHA: testSHA,
		TagSHA:      testSHA,
		Releases: []Release{{
			ID:      1,
			TagName: "v1.2.3",
			Draft:   true,
			Assets:  []Asset{{Name: "manual-extra.zip"}},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "unexpected asset") {
		t.Fatalf("Evaluate error = %v, want unexpected asset", err)
	}
}

func TestEvaluatePublishedReleaseIsReadOnly(t *testing.T) {
	plan, err := Simulate(StatePublished, "115driver", "v1.2.3", testSHA)
	if err != nil {
		t.Fatalf("Simulate: %v", err)
	}
	if plan.State != StatePublished || !plan.ReadOnly || plan.Prerelease || len(plan.MissingAssets) != 0 {
		t.Fatalf("unexpected published plan: %+v", plan)
	}
	if got := strings.Join(plan.Operations, ","); got != "verify-published" {
		t.Fatalf("unexpected operations: %s", got)
	}
}

func TestEvaluatePublishedRejectsMissingAsset(t *testing.T) {
	version, _, err := ParseTag("v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := ExpectedAssets("115driver", version)
	if err != nil {
		t.Fatal(err)
	}
	assets := make([]Asset, 0, len(expected)-1)
	for _, name := range expected[1:] {
		assets = append(assets, Asset{Name: name})
	}
	_, err = Evaluate(Input{
		Tag:         "v1.2.3",
		ExpectedSHA: testSHA,
		TagSHA:      testSHA,
		Releases:    []Release{{ID: 1, TagName: "v1.2.3", Assets: assets}},
	})
	if err == nil || !strings.Contains(err.Error(), "missing expected assets") {
		t.Fatalf("Evaluate error = %v, want missing asset failure", err)
	}
}

func TestEvaluatePublishedRejectsPrereleaseMismatch(t *testing.T) {
	version, _, err := ParseTag("v1.2.3-rc.1")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := ExpectedAssets("115driver", version)
	if err != nil {
		t.Fatal(err)
	}
	assets := make([]Asset, 0, len(expected))
	for _, name := range expected {
		assets = append(assets, Asset{Name: name})
	}
	_, err = Evaluate(Input{
		Tag:         "v1.2.3-rc.1",
		ExpectedSHA: testSHA,
		TagSHA:      testSHA,
		Releases:    []Release{{ID: 1, TagName: "v1.2.3-rc.1", Prerelease: false, Assets: assets}},
	})
	if err == nil || !strings.Contains(err.Error(), "prerelease=false") {
		t.Fatalf("Evaluate error = %v, want prerelease mismatch", err)
	}
}

func TestEvaluateRejectsDuplicateReleaseAndChangedTagTarget(t *testing.T) {
	_, err := Evaluate(Input{
		Tag:         "v1.2.3",
		ExpectedSHA: testSHA,
		TagSHA:      testSHA,
		Releases:    []Release{{ID: 1, TagName: "v1.2.3"}, {ID: 2, TagName: "v1.2.3"}},
	})
	if err == nil || !strings.Contains(err.Error(), "2 GitHub releases") {
		t.Fatalf("Evaluate error = %v, want duplicate release failure", err)
	}

	_, err = Evaluate(Input{Tag: "v1.2.3", ExpectedSHA: testSHA, TagSHA: "1123456789abcdef0123456789abcdef01234567"})
	if err == nil || !strings.Contains(err.Error(), "want source") {
		t.Fatalf("Evaluate error = %v, want changed tag target failure", err)
	}
}

func TestParseTagRequiresSemVer(t *testing.T) {
	for _, tag := range []string{"1.2.3", "v1.2", "v01.2.3", "v1.2.3/evil", "v1.2.3-01", "v1.2.3-rc.01"} {
		if _, _, err := ParseTag(tag); err == nil {
			t.Fatalf("ParseTag(%q) unexpectedly succeeded", tag)
		}
	}
	for _, tag := range []string{"v1.2.3-0", "v1.2.3-rc.0", "v1.2.3-rc.01a", "v1.2.3+build.01"} {
		if _, _, err := ParseTag(tag); err != nil {
			t.Fatalf("ParseTag(%q) returned error: %v", tag, err)
		}
	}
}

func TestEvaluateRequiresNewCandidateToAdvancePublishedLineage(t *testing.T) {
	for _, tag := range []string{"v0.1.3", "v0.1.4-rc.1", "v0.1.4+rebuild.1"} {
		_, err := Evaluate(Input{
			Tag:         tag,
			ExpectedSHA: testSHA,
			TagSHA:      testSHA,
			Releases:    []Release{{ID: 1, TagName: "v0.1.4"}},
		})
		if err == nil || !strings.Contains(err.Error(), "does not advance published release lineage after v0.1.4") {
			t.Fatalf("Evaluate(%q) error = %v, want lineage rejection", tag, err)
		}
	}
}

func TestEvaluateRCAdvancesStableLineage(t *testing.T) {
	plan, err := Evaluate(Input{
		Tag:         "v0.2.0-rc.1",
		ExpectedSHA: testSHA,
		TagSHA:      testSHA,
		Releases: []Release{
			{ID: 1, TagName: "v0.1.3"},
			{ID: 2, TagName: "v0.1.4"},
			{ID: 3, TagName: "v9.0.0", Draft: true},
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if plan.LatestPublishedTag != "v0.1.4" || plan.PromotionFrom != "" || !plan.Prerelease {
		t.Fatalf("unexpected lineage plan: %+v", plan)
	}
}

func TestEvaluateRCSequenceUsesSemVerPrecedence(t *testing.T) {
	plan, err := Evaluate(Input{
		Tag:         "v0.2.0-rc.10",
		ExpectedSHA: testSHA,
		TagSHA:      testSHA,
		Releases: []Release{
			{ID: 1, TagName: "v0.2.0-rc.2", Prerelease: true},
			{ID: 2, TagName: "v0.2.0-rc.9", Prerelease: true},
		},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if plan.LatestPublishedTag != "v0.2.0-rc.9" {
		t.Fatalf("latest published tag = %q, want v0.2.0-rc.9", plan.LatestPublishedTag)
	}
}

func TestEvaluateStablePromotionRequiresSameActivePrereleaseCore(t *testing.T) {
	plan, err := Evaluate(Input{
		Tag:         "v0.2.0",
		ExpectedSHA: testSHA,
		TagSHA:      testSHA,
		Releases:    []Release{{ID: 1, TagName: "v0.2.0-rc.2", Prerelease: true}},
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if plan.LatestPublishedTag != "v0.2.0-rc.2" || plan.PromotionFrom != "v0.2.0-rc.2" || plan.Prerelease {
		t.Fatalf("unexpected stable promotion plan: %+v", plan)
	}

	_, err = Evaluate(Input{
		Tag:         "v0.2.1",
		ExpectedSHA: testSHA,
		TagSHA:      testSHA,
		Releases:    []Release{{ID: 1, TagName: "v0.2.0-rc.2", Prerelease: true}},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot cross active prerelease line v0.2.0-rc.2") {
		t.Fatalf("cross-line stable promotion error = %v", err)
	}
}

func TestEvaluatePublishedRerunMayVerifyOlderRelease(t *testing.T) {
	version, _, err := ParseTag("v0.1.4")
	if err != nil {
		t.Fatal(err)
	}
	expected, err := ExpectedAssets("115driver", version)
	if err != nil {
		t.Fatal(err)
	}
	assets := make([]Asset, 0, len(expected))
	for _, name := range expected {
		assets = append(assets, Asset{Name: name})
	}
	plan, err := Evaluate(Input{
		Tag:         "v0.1.4",
		ExpectedSHA: testSHA,
		TagSHA:      testSHA,
		Releases: []Release{
			{ID: 1, TagName: "v0.1.4", Assets: assets},
			{ID: 2, TagName: "v0.2.0", Assets: assets},
		},
	})
	if err != nil {
		t.Fatalf("Evaluate published rerun: %v", err)
	}
	if plan.State != StatePublished || !plan.ReadOnly || plan.LatestPublishedTag != "" {
		t.Fatalf("unexpected published rerun plan: %+v", plan)
	}
}

func TestSimulateWithReleasesUsesRealPublishedBaseline(t *testing.T) {
	baseline := []Release{{ID: 1, TagName: "v0.1.4"}}
	for _, state := range []State{StateAbsent, StateDraft, StatePublished} {
		plan, err := SimulateWithReleases(state, "115driver", "v0.2.0-rc.1", testSHA, baseline)
		if err != nil {
			t.Fatalf("SimulateWithReleases(%s): %v", state, err)
		}
		if state != StatePublished && plan.LatestPublishedTag != "v0.1.4" {
			t.Fatalf("state %s latest published tag = %q, want v0.1.4", state, plan.LatestPublishedTag)
		}
	}
}

func TestEvaluateRejectsInvalidPublishedVTagInLineage(t *testing.T) {
	_, err := Evaluate(Input{
		Tag:         "v0.2.0-rc.1",
		ExpectedSHA: testSHA,
		TagSHA:      testSHA,
		Releases:    []Release{{ID: 1, TagName: "v0.1"}},
	})
	if err == nil || !strings.Contains(err.Error(), "published v-prefixed release tag") {
		t.Fatalf("Evaluate error = %v, want malformed published lineage failure", err)
	}
}

func TestEvaluateRejectsHistoricalPrereleaseMetadataDrift(t *testing.T) {
	_, err := Evaluate(Input{
		Tag:         "v0.2.0-rc.2",
		ExpectedSHA: testSHA,
		TagSHA:      testSHA,
		Releases:    []Release{{ID: 1, TagName: "v0.2.0-rc.1", Prerelease: false}},
	})
	if err == nil || !strings.Contains(err.Error(), "SemVer tag requires prerelease=true") {
		t.Fatalf("Evaluate error = %v, want historical prerelease metadata drift failure", err)
	}
}

func TestEvaluateRejectsDuplicatePublishedSemVerPrecedence(t *testing.T) {
	_, err := Evaluate(Input{
		Tag:         "v0.2.0-rc.1",
		ExpectedSHA: testSHA,
		TagSHA:      testSHA,
		Releases: []Release{
			{ID: 1, TagName: "v0.1.4+build.1"},
			{ID: 2, TagName: "v0.1.4+build.2"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate SemVer precedence") {
		t.Fatalf("Evaluate error = %v, want duplicate precedence failure", err)
	}
}
