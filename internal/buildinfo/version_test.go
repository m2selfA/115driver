package buildinfo

import "testing"

func TestResolveVersionPrefersLinkedRelease(t *testing.T) {
	for _, linked := range []string{"1.4.0", "v1.4.0"} {
		if got := resolveVersion(linked, "v1.3.0"); got != "1.4.0" {
			t.Fatalf("resolveVersion linked release %q = %q, want 1.4.0", linked, got)
		}
	}
}

func TestResolveVersionUsesModuleVersionForGoInstallBuild(t *testing.T) {
	if got := resolveVersion("dev", "v1.3.0"); got != "1.3.0" {
		t.Fatalf("resolveVersion module release = %q, want 1.3.0", got)
	}
}

func TestResolveVersionPreservesDevelopmentFallback(t *testing.T) {
	for _, moduleVersion := range []string{"", "(devel)"} {
		if got := resolveVersion("dev", moduleVersion); got != "dev" {
			t.Fatalf("resolveVersion development fallback for %q = %q, want dev", moduleVersion, got)
		}
	}
}
