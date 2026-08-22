package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/SheltonZhu/115driver/internal/releaseops"
)

func main() {
	project := flag.String("project", "115driver", "GoReleaser project/archive name")
	tag := flag.String("tag", "", "release tag in vMAJOR.MINOR.PATCH form")
	expectedSHA := flag.String("expected-sha", "", "source commit SHA expected for the release tag")
	tagSHA := flag.String("tag-sha", "", "commit SHA currently targeted by the release tag; defaults to expected-sha")
	releasesFile := flag.String("releases-file", "", "JSON array of GitHub release objects, or - for stdin")
	simulatedState := flag.String("simulate-state", "", "simulate absent, draft, or published without reading GitHub release JSON")
	flag.Parse()

	if strings.TrimSpace(*tag) == "" || strings.TrimSpace(*expectedSHA) == "" {
		log.Fatal("-tag and -expected-sha are required")
	}
	resolvedTagSHA := strings.TrimSpace(*tagSHA)
	if resolvedTagSHA == "" {
		resolvedTagSHA = strings.TrimSpace(*expectedSHA)
	}

	var (
		plan     releaseops.Plan
		err      error
		releases []releaseops.Release
	)
	if strings.TrimSpace(*releasesFile) != "" {
		var loadErr error
		releases, loadErr = loadReleases(*releasesFile)
		if loadErr != nil {
			log.Fatal(loadErr)
		}
	}
	if strings.TrimSpace(*simulatedState) != "" {
		plan, err = releaseops.SimulateWithReleases(releaseops.State(strings.TrimSpace(*simulatedState)), *project, *tag, resolvedTagSHA, releases)
	} else {
		if strings.TrimSpace(*releasesFile) == "" {
			log.Fatal("one of -simulate-state or -releases-file is required")
		}
		plan, err = releaseops.Evaluate(releaseops.Input{
			ProjectName: *project,
			Tag:         *tag,
			ExpectedSHA: *expectedSHA,
			TagSHA:      resolvedTagSHA,
			Releases:    releases,
		})
	}
	if err != nil {
		log.Fatal(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(plan); err != nil {
		log.Fatal(err)
	}
}

func loadReleases(path string) ([]releaseops.Release, error) {
	var reader io.Reader
	if path == "-" {
		reader = os.Stdin
	} else {
		file, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open releases JSON: %w", err)
		}
		defer file.Close()
		reader = file
	}
	body, err := io.ReadAll(io.LimitReader(reader, 32<<20))
	if err != nil {
		return nil, fmt.Errorf("read releases JSON: %w", err)
	}
	var releases []releaseops.Release
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, fmt.Errorf("decode releases JSON: %w", err)
	}
	return releases, nil
}
