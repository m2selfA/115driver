package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/SheltonZhu/115driver/internal/releaseops"
)

func main() {
	manifestPath := flag.String("manifest", "V0.2.0_RC1_COMMIT_MANIFEST.json", "RC commit-boundary manifest, relative to repository root unless absolute")
	printLayer := flag.String("print-layer", "", "print the sorted paths for one verified layer")
	printLayerNUL := flag.Bool("print-layer-nul", false, "emit -print-layer paths as NUL-delimited pathspec data")
	verifyIndexLayer := flag.String("verify-index-layer", "", "verify the Git index contains exactly one frozen layer; use empty to require no staged paths")
	flag.Parse()
	if flag.NArg() != 0 {
		log.Fatalf("unexpected positional arguments: %v", flag.Args())
	}
	if *printLayerNUL && strings.TrimSpace(*printLayer) == "" {
		log.Fatal("-print-layer-nul requires -print-layer")
	}
	if strings.TrimSpace(*printLayer) != "" && strings.TrimSpace(*verifyIndexLayer) != "" {
		log.Fatal("-print-layer and -verify-index-layer are mutually exclusive")
	}

	root, err := gitOutput("", "rev-parse", "--show-toplevel")
	if err != nil {
		log.Fatal(err)
	}
	root = strings.TrimSpace(root)
	if root == "" {
		log.Fatal("git repository root is empty")
	}
	resolvedManifest := *manifestPath
	if !filepath.IsAbs(resolvedManifest) {
		resolvedManifest = filepath.Join(root, resolvedManifest)
	}
	manifest, err := loadManifest(resolvedManifest)
	if err != nil {
		log.Fatal(err)
	}
	if err := releaseops.ValidateCommitManifest(manifest); err != nil {
		log.Fatal(err)
	}
	if _, err := gitOutput(root, "rev-parse", "--verify", manifest.BaseTag+"^{commit}"); err != nil {
		log.Fatalf("resolve base tag %s: %v", manifest.BaseTag, err)
	}
	if err := gitRun(root, "merge-base", "--is-ancestor", manifest.BaseTag, "HEAD"); err != nil {
		log.Fatalf("base tag %s is not an ancestor of HEAD: %v", manifest.BaseTag, err)
	}

	candidatePaths, err := collectCandidatePaths(root, manifest.BaseTag)
	if err != nil {
		log.Fatal(err)
	}
	report, err := releaseops.EvaluateCommitBoundary(manifest, candidatePaths)
	if err != nil {
		log.Fatal(err)
	}
	if layer := strings.TrimSpace(*verifyIndexLayer); layer != "" {
		stagedPaths, err := collectStagedPaths(root)
		if err != nil {
			log.Fatal(err)
		}
		if layer == "empty" {
			if len(stagedPaths) != 0 {
				log.Fatalf("RC Git index is not empty: staged_paths=%d", len(stagedPaths))
			}
			fmt.Println("verified RC Git index empty")
			return
		}
		indexReport, err := releaseops.EvaluateCommitIndexLayer(report, layer, stagedPaths)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("verified RC Git index layer=%s paths=%d sha256=%s subject=%q\n", indexReport.ID, indexReport.Count, indexReport.PathSetSHA256, indexReport.Subject)
		return
	}
	if layer := strings.TrimSpace(*printLayer); layer != "" {
		for _, item := range report.Layers {
			if item.ID == layer {
				for _, itemPath := range item.Paths {
					if *printLayerNUL {
						if _, err := os.Stdout.Write([]byte(itemPath + "\x00")); err != nil {
							log.Fatal(err)
						}
					} else {
						fmt.Println(itemPath)
					}
				}
				return
			}
		}
		log.Fatalf("unknown RC commit layer %q", layer)
	}

	fmt.Printf("verified RC commit boundary: base=%s candidate=%s layers=%d paths=%d sha256=%s\n", report.BaseTag, report.CandidateTag, len(report.Layers), report.TotalPaths, report.PathSetSHA256)
	for _, layer := range report.Layers {
		fmt.Printf("layer=%s paths=%d sha256=%s subject=%q\n", layer.ID, layer.Count, layer.PathSetSHA256, layer.Subject)
	}
}

func loadManifest(path string) (releaseops.CommitManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return releaseops.CommitManifest{}, fmt.Errorf("open RC commit manifest: %w", err)
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var manifest releaseops.CommitManifest
	if err := decoder.Decode(&manifest); err != nil {
		return releaseops.CommitManifest{}, fmt.Errorf("decode RC commit manifest: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return releaseops.CommitManifest{}, err
	}
	return manifest, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("RC commit manifest contains trailing JSON")
		}
		return fmt.Errorf("decode trailing RC commit manifest data: %w", err)
	}
	return nil
}

func collectCandidatePaths(root, baseTag string) ([]string, error) {
	set := make(map[string]struct{})
	queries := [][]string{
		{"diff", "--name-only", "--no-renames", baseTag + "..HEAD", "--"},
		{"diff", "--name-only", "--no-renames", "HEAD", "--"},
		{"ls-files", "--others", "--exclude-standard"},
	}
	for _, args := range queries {
		output, err := gitOutput(root, args...)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(output, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				set[line] = struct{}{}
			}
		}
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	return paths, nil
}

func collectStagedPaths(root string) ([]string, error) {
	output, err := gitOutput(root, "diff", "--cached", "--name-only", "--no-renames", "--")
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths, nil
}

func gitOutput(root string, args ...string) (string, error) {
	commandArgs := append([]string(nil), args...)
	cmd := exec.Command("git", commandArgs...)
	if root != "" {
		cmd.Dir = root
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(output), nil
}

func gitRun(root string, args ...string) error {
	cmd := exec.Command("git", args...)
	if root != "" {
		cmd.Dir = root
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
