package releaseartifact

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

const defaultProjectName = "115driver"

type Options struct {
	DistDir         string
	ProjectName     string
	ExpectedVersion string
	SmokeHost       bool
}

type Report struct {
	Version       string
	ArchiveCount  int
	SBOMCount     int
	HostSmoked    bool
	InstallSmoked bool
}

type target struct {
	goos        string
	goarch      string
	archiveArch string
}

var releaseTargets = []target{
	{goos: "darwin", goarch: "amd64", archiveArch: "x86_64"},
	{goos: "darwin", goarch: "arm64", archiveArch: "aarch64"},
	{goos: "linux", goarch: "amd64", archiveArch: "x86_64"},
	{goos: "linux", goarch: "arm64", archiveArch: "aarch64"},
	{goos: "windows", goarch: "amd64", archiveArch: "x86_64"},
	{goos: "windows", goarch: "arm64", archiveArch: "aarch64"},
}

func Verify(opts Options) (Report, error) {
	distDir := strings.TrimSpace(opts.DistDir)
	if distDir == "" {
		distDir = "dist"
	}
	projectName := strings.TrimSpace(opts.ProjectName)
	if projectName == "" {
		projectName = defaultProjectName
	}
	if err := verifyArchiveSet(distDir, projectName); err != nil {
		return Report{}, err
	}

	checksums, err := readChecksums(filepath.Join(distDir, "checksums.txt"))
	if err != nil {
		return Report{}, err
	}

	archives := make(map[target]string, len(releaseTargets))
	version := ""
	for _, releaseTarget := range releaseTargets {
		name, targetVersion, err := findArchive(distDir, projectName, releaseTarget)
		if err != nil {
			return Report{}, err
		}
		if version == "" {
			version = targetVersion
		} else if targetVersion != version {
			return Report{}, fmt.Errorf("release archive versions disagree: %q and %q", version, targetVersion)
		}
		archives[releaseTarget] = name
	}
	if version == "" || version == "dev" {
		return Report{}, fmt.Errorf("invalid release artifact version %q", version)
	}
	if err := verifyManifestSet("checksums.txt", checksums, archives); err != nil {
		return Report{}, err
	}
	if err := verifySBOMSet(distDir, archives); err != nil {
		return Report{}, err
	}
	expectedVersion := strings.TrimPrefix(strings.TrimSpace(opts.ExpectedVersion), "v")
	if expectedVersion != "" && version != expectedVersion {
		return Report{}, fmt.Errorf("release artifact version = %q, want %q", version, expectedVersion)
	}

	var hostArchive string
	var hostTarget target
	for releaseTarget, name := range archives {
		archivePath := filepath.Join(distDir, name)
		if err := verifyChecksum(archivePath, name, checksums); err != nil {
			return Report{}, err
		}
		members, err := inspectArchive(archivePath)
		if err != nil {
			return Report{}, err
		}
		if err := verifyMembers(name, releaseTarget.goos, members); err != nil {
			return Report{}, err
		}
		sbomName := name + ".spdx.json"
		if err := verifyChecksum(filepath.Join(distDir, sbomName), sbomName, checksums); err != nil {
			return Report{}, err
		}
		if err := verifySPDXSBOM(filepath.Join(distDir, sbomName)); err != nil {
			return Report{}, err
		}
		if opts.SmokeHost && releaseTarget.goos == runtime.GOOS && releaseTarget.goarch == runtime.GOARCH {
			hostArchive = archivePath
			hostTarget = releaseTarget
		}
	}

	report := Report{Version: version, ArchiveCount: len(archives), SBOMCount: len(archives)}
	if opts.SmokeHost {
		if hostArchive == "" {
			return Report{}, fmt.Errorf("release matrix has no archive for host %s/%s", runtime.GOOS, runtime.GOARCH)
		}
		if err := smokeHostArchive(hostArchive, hostTarget.goos, version); err != nil {
			return Report{}, err
		}
		report.HostSmoked = true
		report.InstallSmoked = true
	}
	return report, nil
}

func verifyArchiveSet(distDir, projectName string) error {
	entries, err := os.ReadDir(distDir)
	if err != nil {
		return fmt.Errorf("read release dist: %w", err)
	}
	prefix := projectName + "_"
	count := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".tar.gz") {
			count++
		}
	}
	if count != len(releaseTargets) {
		return fmt.Errorf("release archive set has %d project archives, want %d", count, len(releaseTargets))
	}
	return nil
}

func verifySBOMSet(distDir string, archives map[target]string) error {
	entries, err := os.ReadDir(distDir)
	if err != nil {
		return fmt.Errorf("read release dist: %w", err)
	}
	want := make(map[string]struct{}, len(archives))
	for _, archiveName := range archives {
		want[archiveName+".spdx.json"] = struct{}{}
	}
	got := make(map[string]struct{}, len(want))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".tar.gz.spdx.json") {
			continue
		}
		got[entry.Name()] = struct{}{}
	}
	if len(got) != len(want) {
		return fmt.Errorf("release SBOM set has %d documents, want %d", len(got), len(want))
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			return fmt.Errorf("release SBOM set does not contain %q", name)
		}
	}
	return nil
}

func verifyManifestSet(manifest string, checksums map[string]string, archives map[target]string) error {
	want := make(map[string]struct{}, len(archives)*2)
	for _, archiveName := range archives {
		want[archiveName] = struct{}{}
		want[archiveName+".spdx.json"] = struct{}{}
	}
	if len(checksums) != len(want) {
		return fmt.Errorf("%s has %d entries, want %d", manifest, len(checksums), len(want))
	}
	for name := range want {
		if _, ok := checksums[name]; !ok {
			return fmt.Errorf("%s does not contain %q", manifest, name)
		}
	}
	return nil
}

type spdxDocument struct {
	SPDXVersion       string `json:"spdxVersion"`
	DataLicense       string `json:"dataLicense"`
	SPDXID            string `json:"SPDXID"`
	Name              string `json:"name"`
	DocumentNamespace string `json:"documentNamespace"`
	CreationInfo      struct {
		Creators []string `json:"creators"`
	} `json:"creationInfo"`
}

func verifySPDXSBOM(pathName string) error {
	body, err := os.ReadFile(pathName)
	if err != nil {
		return fmt.Errorf("read release SBOM %q: %w", filepath.Base(pathName), err)
	}
	var doc spdxDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("decode release SBOM %q: %w", filepath.Base(pathName), err)
	}
	if doc.SPDXVersion != "SPDX-2.3" {
		return fmt.Errorf("release SBOM %q spdxVersion = %q, want SPDX-2.3", filepath.Base(pathName), doc.SPDXVersion)
	}
	if doc.DataLicense != "CC0-1.0" {
		return fmt.Errorf("release SBOM %q dataLicense = %q, want CC0-1.0", filepath.Base(pathName), doc.DataLicense)
	}
	if doc.SPDXID != "SPDXRef-DOCUMENT" {
		return fmt.Errorf("release SBOM %q SPDXID = %q, want SPDXRef-DOCUMENT", filepath.Base(pathName), doc.SPDXID)
	}
	if strings.TrimSpace(doc.Name) == "" || strings.TrimSpace(doc.DocumentNamespace) == "" || len(doc.CreationInfo.Creators) == 0 {
		return fmt.Errorf("release SBOM %q is missing required document metadata", filepath.Base(pathName))
	}
	return nil
}

func findArchive(distDir, projectName string, releaseTarget target) (string, string, error) {
	entries, err := os.ReadDir(distDir)
	if err != nil {
		return "", "", fmt.Errorf("read release dist: %w", err)
	}
	prefix := projectName + "_"
	suffix := "_" + releaseTarget.goos + "_" + releaseTarget.archiveArch + ".tar.gz"
	matches := make([]string, 0, 1)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix) {
			matches = append(matches, name)
		}
	}
	if len(matches) != 1 {
		return "", "", fmt.Errorf("release target %s/%s has %d matching archives, want 1", releaseTarget.goos, releaseTarget.goarch, len(matches))
	}
	name := matches[0]
	version := strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix)
	if strings.TrimSpace(version) == "" {
		return "", "", fmt.Errorf("release archive %q has empty version", name)
	}
	return name, version, nil
}

func readChecksums(pathName string) (map[string]string, error) {
	file, err := os.Open(pathName)
	if err != nil {
		return nil, fmt.Errorf("open release checksums: %w", err)
	}
	defer file.Close()

	checksums := make(map[string]string)
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid checksum line %q", scanner.Text())
		}
		checksum := strings.ToLower(fields[0])
		if len(checksum) != sha256.Size*2 {
			return nil, fmt.Errorf("invalid SHA-256 checksum for %q", fields[1])
		}
		if _, err := hex.DecodeString(checksum); err != nil {
			return nil, fmt.Errorf("invalid SHA-256 checksum for %q: %w", fields[1], err)
		}
		if _, exists := checksums[fields[1]]; exists {
			return nil, fmt.Errorf("duplicate checksum entry for %q", fields[1])
		}
		checksums[fields[1]] = checksum
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read release checksums: %w", err)
	}
	return checksums, nil
}

func verifyChecksum(pathName, name string, checksums map[string]string) error {
	want, ok := checksums[name]
	if !ok {
		return fmt.Errorf("checksums.txt does not contain %q", name)
	}
	file, err := os.Open(pathName)
	if err != nil {
		return fmt.Errorf("open release archive %q: %w", name, err)
	}
	defer file.Close()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return fmt.Errorf("hash release archive %q: %w", name, err)
	}
	got := hex.EncodeToString(h.Sum(nil))
	if got != want {
		return fmt.Errorf("checksum mismatch for %q: got %s, want %s", name, got, want)
	}
	return nil
}

func inspectArchive(archivePath string) (map[string][]byte, error) {
	file, err := os.Open(archivePath)
	if err != nil {
		return nil, fmt.Errorf("open release archive %q: %w", filepath.Base(archivePath), err)
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return nil, fmt.Errorf("open gzip archive %q: %w", filepath.Base(archivePath), err)
	}
	defer gz.Close()

	members := make(map[string][]byte)
	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive %q: %w", filepath.Base(archivePath), err)
		}
		if header.FileInfo().IsDir() {
			return nil, fmt.Errorf("archive %q unexpectedly contains directory %q", filepath.Base(archivePath), header.Name)
		}
		if !header.FileInfo().Mode().IsRegular() {
			return nil, fmt.Errorf("archive %q contains non-regular member %q", filepath.Base(archivePath), header.Name)
		}
		clean := path.Clean(header.Name)
		if path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") || clean != path.Base(clean) {
			return nil, fmt.Errorf("archive %q contains unsafe or wrapped member %q", filepath.Base(archivePath), header.Name)
		}
		if _, exists := members[clean]; exists {
			return nil, fmt.Errorf("archive %q contains duplicate member %q", filepath.Base(archivePath), clean)
		}
		body, err := io.ReadAll(io.LimitReader(tr, 256<<20))
		if err != nil {
			return nil, fmt.Errorf("read archive member %q: %w", clean, err)
		}
		if int64(len(body)) != header.Size {
			return nil, fmt.Errorf("archive member %q size mismatch", clean)
		}
		members[clean] = body
	}
	return members, nil
}

func verifyMembers(archiveName, goos string, members map[string][]byte) error {
	exe := ""
	if goos == "windows" {
		exe = ".exe"
	}
	binaries := []string{"115driver" + exe, "115driver-mcp-server" + exe}
	want := append([]string{}, binaries...)
	want = append(want, "LICENSE", "README.md")
	sort.Strings(want)
	got := make([]string, 0, len(members))
	for name := range members {
		got = append(got, name)
	}
	sort.Strings(got)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		return fmt.Errorf("archive %q members = %v, want %v", archiveName, got, want)
	}
	for _, binary := range binaries {
		if len(members[binary]) == 0 {
			return fmt.Errorf("archive %q contains empty binary %q", archiveName, binary)
		}
	}
	return nil
}

func smokeHostArchive(archivePath, goos, version string) error {
	members, err := inspectArchive(archivePath)
	if err != nil {
		return err
	}
	exe := ""
	if goos == "windows" {
		exe = ".exe"
	}
	tmp, err := os.MkdirTemp("", "115driver-release-install-")
	if err != nil {
		return fmt.Errorf("create artifact install smoke directory: %w", err)
	}
	defer os.RemoveAll(tmp)
	installDir := filepath.Join(tmp, "115driver")
	if err := os.Mkdir(installDir, 0o700); err != nil {
		return fmt.Errorf("create artifact install prefix: %w", err)
	}

	paths := make(map[string]string, 2)
	for name, body := range members {
		mode := os.FileMode(0o600)
		if name == "115driver"+exe || name == "115driver-mcp-server"+exe {
			mode = 0o700
		}
		installedPath := filepath.Join(installDir, name)
		if err := os.WriteFile(installedPath, body, mode); err != nil {
			return fmt.Errorf("install archive member %q: %w", name, err)
		}
		if mode&0o100 != 0 {
			paths[name] = installedPath
		}
	}

	if err := exactOutput(paths["115driver"+exe], []string{"--version"}, "115driver version "+version+"\n"); err != nil {
		return err
	}
	if err := exactOutput(paths["115driver"+exe], []string{"version"}, "115driver version "+version+"\n"); err != nil {
		return err
	}
	if err := exactOutput(paths["115driver-mcp-server"+exe], []string{"--version"}, "115driver-mcp-server "+version+"\n"); err != nil {
		return err
	}
	if err := helpOutput(paths["115driver"+exe], "CLI tool for 115 cloud storage"); err != nil {
		return err
	}
	if err := helpOutput(paths["115driver-mcp-server"+exe], "Usage: 115driver-mcp-server [OPTIONS]"); err != nil {
		return err
	}
	return nil
}

func exactOutput(binary string, args []string, want string) error {
	cmd := exec.Command(binary, args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("run %s %s: %w: %s", filepath.Base(binary), strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	if string(output) != want {
		return fmt.Errorf("run %s %s output = %q, want %q", filepath.Base(binary), strings.Join(args, " "), string(output), want)
	}
	return nil
}

func helpOutput(binary, needle string) error {
	cmd := exec.Command(binary, "--help")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("run %s --help: %w: %s", filepath.Base(binary), err, strings.TrimSpace(string(output)))
	}
	if !strings.Contains(string(output), needle) {
		return fmt.Errorf("run %s --help did not contain %q", filepath.Base(binary), needle)
	}
	return nil
}
