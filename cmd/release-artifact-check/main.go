package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/SheltonZhu/115driver/internal/releaseartifact"
)

func main() {
	dist := flag.String("dist", "dist", "GoReleaser dist directory")
	project := flag.String("project", "115driver", "GoReleaser project/archive name")
	expectedVersion := flag.String("expected-version", "", "required artifact version; an optional leading v is ignored")
	smokeHost := flag.Bool("smoke-host", true, "execute host-architecture binaries from the release archive")
	flag.Parse()

	report, err := releaseartifact.Verify(releaseartifact.Options{
		DistDir:         *dist,
		ProjectName:     *project,
		ExpectedVersion: *expectedVersion,
		SmokeHost:       *smokeHost,
	})
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("verified GoReleaser artifacts: version=%s archives=%d sboms=%d host_smoke=%t install_smoke=%t\n", report.Version, report.ArchiveCount, report.SBOMCount, report.HostSmoked, report.InstallSmoked)
}
