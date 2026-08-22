package buildinfo

import (
	"runtime/debug"
	"strings"
)

const DevelopmentVersion = "dev"

// Version returns an explicitly linked version when present, otherwise it
// falls back to the Go module version embedded in binaries produced by
// `go install module@version`. Local development builds return "dev".
func Version(linked string) string {
	moduleVersion := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		moduleVersion = info.Main.Version
	}
	return resolveVersion(linked, moduleVersion)
}

func resolveVersion(linked, moduleVersion string) string {
	linked = strings.TrimSpace(linked)
	if linked != "" && linked != DevelopmentVersion {
		return strings.TrimPrefix(linked, "v")
	}

	moduleVersion = strings.TrimSpace(moduleVersion)
	if moduleVersion != "" && moduleVersion != "(devel)" {
		return strings.TrimPrefix(moduleVersion, "v")
	}
	if linked != "" {
		return linked
	}
	return DevelopmentVersion
}
