package releaseops

import (
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

type State string

const (
	StateAbsent    State = "absent"
	StateDraft     State = "draft"
	StatePublished State = "published"
)

type Asset struct {
	Name string `json:"name"`
}

type Release struct {
	ID         int64   `json:"id"`
	TagName    string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}

type Input struct {
	ProjectName string
	Tag         string
	ExpectedSHA string
	TagSHA      string
	Releases    []Release
}

type Plan struct {
	State                 State    `json:"state"`
	Tag                   string   `json:"tag"`
	Version               string   `json:"version"`
	Prerelease            bool     `json:"prerelease"`
	ReleaseID             int64    `json:"release_id,omitempty"`
	LatestPublishedTag    string   `json:"latest_published_tag,omitempty"`
	PromotionFrom         string   `json:"promotion_from,omitempty"`
	ReadOnly              bool     `json:"read_only"`
	PrereleaseNeedsUpdate bool     `json:"prerelease_needs_update,omitempty"`
	ExpectedAssets        []string `json:"expected_assets"`
	MissingAssets         []string `json:"missing_assets,omitempty"`
	Operations            []string `json:"operations"`
}

type semanticVersion struct {
	tag        string
	major      string
	minor      string
	patch      string
	prerelease []string
}

var semverTagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

var releaseTargetNames = []string{
	"darwin_aarch64",
	"darwin_x86_64",
	"linux_aarch64",
	"linux_x86_64",
	"windows_aarch64",
	"windows_x86_64",
}

func ParseTag(tag string) (version string, prerelease bool, err error) {
	parsed, err := parseSemanticTag(tag)
	if err != nil {
		return "", false, err
	}
	return strings.TrimPrefix(parsed.tag, "v"), len(parsed.prerelease) != 0, nil
}

func parseSemanticTag(tag string) (semanticVersion, error) {
	tag = strings.TrimSpace(tag)
	match := semverTagPattern.FindStringSubmatch(tag)
	if match == nil {
		return semanticVersion{}, fmt.Errorf("release tag %q must be SemVer in vMAJOR.MINOR.PATCH form with optional prerelease/build metadata", tag)
	}
	prereleaseText := strings.TrimPrefix(match[4], "-")
	prerelease := []string(nil)
	if prereleaseText != "" {
		prerelease = strings.Split(prereleaseText, ".")
		for _, identifier := range prerelease {
			if len(identifier) > 1 && identifier[0] == '0' && allDigits(identifier) {
				return semanticVersion{}, fmt.Errorf("release tag %q has a numeric prerelease identifier with a leading zero", tag)
			}
		}
	}
	return semanticVersion{
		tag:        tag,
		major:      match[1],
		minor:      match[2],
		patch:      match[3],
		prerelease: prerelease,
	}, nil
}

func allDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

func ExpectedAssets(projectName, version string) ([]string, error) {
	projectName = strings.TrimSpace(projectName)
	version = strings.TrimSpace(version)
	if projectName == "" || version == "" {
		return nil, fmt.Errorf("project name and version are required")
	}
	if strings.ContainsAny(projectName, "/\\\t\r\n ") || strings.ContainsAny(version, "/\\\t\r\n ") {
		return nil, fmt.Errorf("project name and version must be safe release filename components")
	}
	assets := make([]string, 0, len(releaseTargetNames)*2+1)
	for _, target := range releaseTargetNames {
		archive := projectName + "_" + version + "_" + target + ".tar.gz"
		assets = append(assets, archive, archive+".spdx.json")
	}
	assets = append(assets, "checksums.txt")
	sort.Strings(assets)
	return assets, nil
}

func Evaluate(input Input) (Plan, error) {
	projectName := strings.TrimSpace(input.ProjectName)
	if projectName == "" {
		projectName = "115driver"
	}
	proposed, err := parseSemanticTag(input.Tag)
	if err != nil {
		return Plan{}, err
	}
	version, prerelease := strings.TrimPrefix(proposed.tag, "v"), len(proposed.prerelease) != 0
	if err := validateSHA("expected source", input.ExpectedSHA); err != nil {
		return Plan{}, err
	}
	if err := validateSHA("tag target", input.TagSHA); err != nil {
		return Plan{}, err
	}
	if !strings.EqualFold(strings.TrimSpace(input.ExpectedSHA), strings.TrimSpace(input.TagSHA)) {
		return Plan{}, fmt.Errorf("release tag %s resolves to %s, want source %s", input.Tag, input.TagSHA, input.ExpectedSHA)
	}
	expectedAssets, err := ExpectedAssets(projectName, version)
	if err != nil {
		return Plan{}, err
	}

	matching := make([]Release, 0, 1)
	for _, release := range input.Releases {
		if release.TagName == input.Tag {
			matching = append(matching, release)
		}
	}
	if len(matching) > 1 {
		return Plan{}, fmt.Errorf("release tag %s has %d GitHub releases, want at most one", input.Tag, len(matching))
	}

	plan := Plan{
		Tag:            input.Tag,
		Version:        version,
		Prerelease:     prerelease,
		ExpectedAssets: expectedAssets,
	}
	if len(matching) == 1 && !matching[0].Draft {
		release := matching[0]
		if release.ID <= 0 {
			return Plan{}, fmt.Errorf("release tag %s has invalid release id %d", input.Tag, release.ID)
		}
		plan.ReleaseID = release.ID
		assetNames, err := validateAssets(release.Assets, expectedAssets)
		if err != nil {
			return Plan{}, fmt.Errorf("release tag %s: %w", input.Tag, err)
		}
		plan.MissingAssets = missingAssets(expectedAssets, assetNames)
		plan.State = StatePublished
		plan.ReadOnly = true
		if release.Prerelease != prerelease {
			return Plan{}, fmt.Errorf("published release prerelease=%t, want %t for tag %s", release.Prerelease, prerelease, input.Tag)
		}
		if len(plan.MissingAssets) != 0 {
			return Plan{}, fmt.Errorf("published release is missing expected assets: %s", strings.Join(plan.MissingAssets, ", "))
		}
		plan.Operations = []string{"verify-published"}
		return plan, nil
	}

	latestPublishedTag, promotionFrom, err := validatePublishedLineage(proposed, input.Releases)
	if err != nil {
		return Plan{}, err
	}
	plan.LatestPublishedTag = latestPublishedTag
	plan.PromotionFrom = promotionFrom

	if len(matching) == 0 {
		plan.State = StateAbsent
		plan.MissingAssets = append([]string(nil), expectedAssets...)
		plan.Operations = []string{"build-candidate", "attest-candidate", "create-draft", "verify-remote-roundtrip", "publish"}
		return plan, nil
	}

	release := matching[0]
	if release.ID <= 0 {
		return Plan{}, fmt.Errorf("release tag %s has invalid release id %d", input.Tag, release.ID)
	}
	plan.ReleaseID = release.ID
	assetNames, err := validateAssets(release.Assets, expectedAssets)
	if err != nil {
		return Plan{}, fmt.Errorf("release tag %s: %w", input.Tag, err)
	}
	plan.MissingAssets = missingAssets(expectedAssets, assetNames)
	plan.State = StateDraft
	plan.PrereleaseNeedsUpdate = release.Prerelease != prerelease
	plan.Operations = []string{"build-candidate", "attest-candidate", "repair-draft", "verify-remote-roundtrip", "publish"}
	return plan, nil
}

func Simulate(state State, projectName, tag, sha string) (Plan, error) {
	return SimulateWithReleases(state, projectName, tag, sha, nil)
}

func SimulateWithReleases(state State, projectName, tag, sha string, releases []Release) (Plan, error) {
	version, prerelease, err := ParseTag(tag)
	if err != nil {
		return Plan{}, err
	}
	expected, err := ExpectedAssets(projectName, version)
	if err != nil {
		return Plan{}, err
	}
	baseline := make([]Release, 0, len(releases)+1)
	for _, release := range releases {
		if release.TagName != tag {
			baseline = append(baseline, release)
		}
	}
	input := Input{ProjectName: projectName, Tag: tag, ExpectedSHA: sha, TagSHA: sha, Releases: baseline}
	switch state {
	case StateAbsent:
	case StateDraft:
		input.Releases = append(input.Releases, Release{
			ID:         101,
			TagName:    tag,
			Draft:      true,
			Prerelease: !prerelease,
			Assets: []Asset{
				{Name: expected[0]},
				{Name: expected[1]},
				{Name: "checksums.txt"},
			},
		})
	case StatePublished:
		assets := make([]Asset, 0, len(expected))
		for _, name := range expected {
			assets = append(assets, Asset{Name: name})
		}
		input.Releases = append(input.Releases, Release{ID: 202, TagName: tag, Prerelease: prerelease, Assets: assets})
	default:
		return Plan{}, fmt.Errorf("unknown simulated release state %q", state)
	}
	return Evaluate(input)
}

func validatePublishedLineage(proposed semanticVersion, releases []Release) (latestTag, promotionFrom string, err error) {
	var latest *semanticVersion
	precedenceTags := make(map[string]string)
	for _, release := range releases {
		if release.Draft || release.TagName == proposed.tag {
			continue
		}
		tag := strings.TrimSpace(release.TagName)
		if !strings.HasPrefix(tag, "v") {
			continue
		}
		parsed, parseErr := parseSemanticTag(tag)
		if parseErr != nil {
			return "", "", fmt.Errorf("published v-prefixed release tag %q is not valid SemVer: %w", tag, parseErr)
		}
		tagPrerelease := len(parsed.prerelease) != 0
		if release.Prerelease != tagPrerelease {
			return "", "", fmt.Errorf("published release %s prerelease=%t, but its SemVer tag requires prerelease=%t", tag, release.Prerelease, tagPrerelease)
		}
		precedenceKey := semanticPrecedenceKey(parsed)
		if priorTag, exists := precedenceTags[precedenceKey]; exists && priorTag != tag {
			return "", "", fmt.Errorf("published release lineage has duplicate SemVer precedence: %s and %s", priorTag, tag)
		}
		precedenceTags[precedenceKey] = tag
		if latest == nil || compareSemanticVersion(parsed, *latest) > 0 {
			candidate := parsed
			latest = &candidate
			latestTag = tag
		}
	}
	if latest == nil {
		return "", "", nil
	}
	if compareSemanticVersion(proposed, *latest) <= 0 {
		return "", "", fmt.Errorf("release tag %s does not advance published release lineage after %s", proposed.tag, latest.tag)
	}
	if len(latest.prerelease) != 0 && len(proposed.prerelease) == 0 {
		if !sameCore(proposed, *latest) {
			return "", "", fmt.Errorf("stable release %s cannot cross active prerelease line %s; stabilize its core version first", proposed.tag, latest.tag)
		}
		promotionFrom = latest.tag
	}
	return latestTag, promotionFrom, nil
}

func compareSemanticVersion(left, right semanticVersion) int {
	for _, pair := range [][2]string{{left.major, right.major}, {left.minor, right.minor}, {left.patch, right.patch}} {
		if cmp := compareDecimal(pair[0], pair[1]); cmp != 0 {
			return cmp
		}
	}
	if len(left.prerelease) == 0 && len(right.prerelease) == 0 {
		return 0
	}
	if len(left.prerelease) == 0 {
		return 1
	}
	if len(right.prerelease) == 0 {
		return -1
	}
	limit := len(left.prerelease)
	if len(right.prerelease) < limit {
		limit = len(right.prerelease)
	}
	for i := 0; i < limit; i++ {
		leftID, rightID := left.prerelease[i], right.prerelease[i]
		leftNumeric, rightNumeric := allDigits(leftID), allDigits(rightID)
		switch {
		case leftNumeric && rightNumeric:
			if cmp := compareDecimal(leftID, rightID); cmp != 0 {
				return cmp
			}
		case leftNumeric:
			return -1
		case rightNumeric:
			return 1
		default:
			if cmp := strings.Compare(leftID, rightID); cmp != 0 {
				return cmp
			}
		}
	}
	if len(left.prerelease) < len(right.prerelease) {
		return -1
	}
	if len(left.prerelease) > len(right.prerelease) {
		return 1
	}
	return 0
}

func compareDecimal(left, right string) int {
	if len(left) < len(right) {
		return -1
	}
	if len(left) > len(right) {
		return 1
	}
	return strings.Compare(left, right)
}

func sameCore(left, right semanticVersion) bool {
	return left.major == right.major && left.minor == right.minor && left.patch == right.patch
}

func semanticPrecedenceKey(version semanticVersion) string {
	return version.major + "." + version.minor + "." + version.patch + "-" + strings.Join(version.prerelease, ".")
}

func validateSHA(label, value string) error {
	value = strings.TrimSpace(value)
	if len(value) != 40 && len(value) != 64 {
		return fmt.Errorf("%s SHA must contain 40 or 64 hexadecimal characters", label)
	}
	if _, err := hex.DecodeString(value); err != nil {
		return fmt.Errorf("%s SHA is not hexadecimal: %w", label, err)
	}
	return nil
}

func validateAssets(assets []Asset, expected []string) (map[string]struct{}, error) {
	expectedSet := make(map[string]struct{}, len(expected))
	for _, name := range expected {
		expectedSet[name] = struct{}{}
	}
	got := make(map[string]struct{}, len(assets))
	for _, asset := range assets {
		name := strings.TrimSpace(asset.Name)
		if name == "" {
			return nil, fmt.Errorf("release contains an asset with an empty name")
		}
		if _, duplicate := got[name]; duplicate {
			return nil, fmt.Errorf("release contains duplicate asset %q", name)
		}
		if _, allowed := expectedSet[name]; !allowed {
			return nil, fmt.Errorf("release contains unexpected asset %q", name)
		}
		got[name] = struct{}{}
	}
	return got, nil
}

func missingAssets(expected []string, got map[string]struct{}) []string {
	missing := make([]string, 0)
	for _, name := range expected {
		if _, ok := got[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}
