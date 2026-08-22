package releaseartifact

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestVerifyAcceptsCompleteReleaseMatrix(t *testing.T) {
	dist := buildFixture(t, "1.2.3-SNAPSHOT-deadbeef", "")
	report, err := Verify(Options{DistDir: dist, ProjectName: "115driver"})
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.Version != "1.2.3-SNAPSHOT-deadbeef" || report.ArchiveCount != 6 || report.SBOMCount != 6 || report.HostSmoked || report.InstallSmoked {
		t.Fatalf("unexpected report: %+v", report)
	}
}

func TestVerifyRequiresExpectedVersion(t *testing.T) {
	dist := buildFixture(t, "1.2.3", "")
	if _, err := Verify(Options{DistDir: dist, ExpectedVersion: "v1.2.4"}); err == nil || !strings.Contains(err.Error(), "want \"1.2.4\"") {
		t.Fatalf("Verify error = %v, want version mismatch", err)
	}
}

func TestVerifyRejectsUnexpectedArchive(t *testing.T) {
	dist := buildFixture(t, "1.2.3", "")
	source := filepath.Join(dist, "115driver_1.2.3_linux_x86_64.tar.gz")
	body, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "115driver_1.2.3_linux_386.tar.gz"), body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(Options{DistDir: dist}); err == nil || !strings.Contains(err.Error(), "archive set has 7") {
		t.Fatalf("Verify error = %v, want unexpected archive failure", err)
	}
}

func TestVerifyRejectsChecksumMismatch(t *testing.T) {
	dist := buildFixture(t, "1.2.3", "")
	entries, err := os.ReadDir(dist)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tar.gz") {
			file, err := os.OpenFile(filepath.Join(dist, entry.Name()), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write([]byte("tamper")); err != nil {
				file.Close()
				t.Fatal(err)
			}
			file.Close()
			break
		}
	}
	if _, err := Verify(Options{DistDir: dist}); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Verify error = %v, want checksum mismatch", err)
	}
}

func TestVerifyRejectsWrappedOrUnsafeMember(t *testing.T) {
	dist := buildFixture(t, "1.2.3", "../115driver")
	if _, err := Verify(Options{DistDir: dist}); err == nil || !strings.Contains(err.Error(), "unsafe or wrapped member") {
		t.Fatalf("Verify error = %v, want unsafe member", err)
	}
}

func TestVerifyRejectsMissingSBOM(t *testing.T) {
	dist := buildFixture(t, "1.2.3", "")
	entries, err := os.ReadDir(dist)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tar.gz.spdx.json") {
			if err := os.Remove(filepath.Join(dist, entry.Name())); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	if _, err := Verify(Options{DistDir: dist}); err == nil || !strings.Contains(err.Error(), "SBOM set has 5") {
		t.Fatalf("Verify error = %v, want missing SBOM failure", err)
	}
}

func TestVerifyRejectsMalformedSBOM(t *testing.T) {
	dist := buildFixture(t, "1.2.3", "")
	entries, err := os.ReadDir(dist)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tar.gz.spdx.json") {
			if err := os.WriteFile(filepath.Join(dist, entry.Name()), []byte(`{"spdxVersion":"SPDX-2.2"}`), 0o600); err != nil {
				t.Fatal(err)
			}
			break
		}
	}
	rewriteChecksums(t, dist)
	if _, err := Verify(Options{DistDir: dist}); err == nil || !strings.Contains(err.Error(), "want SPDX-2.3") {
		t.Fatalf("Verify error = %v, want malformed SPDX failure", err)
	}
}

func TestVerifyRejectsSBOMChecksumMismatch(t *testing.T) {
	dist := buildFixture(t, "1.2.3", "")
	entries, err := os.ReadDir(dist)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".tar.gz.spdx.json") {
			file, err := os.OpenFile(filepath.Join(dist, entry.Name()), os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := file.Write([]byte("tamper")); err != nil {
				file.Close()
				t.Fatal(err)
			}
			file.Close()
			break
		}
	}
	if _, err := Verify(Options{DistDir: dist}); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("Verify error = %v, want SBOM checksum mismatch", err)
	}
}

func buildFixture(t *testing.T, version, cliNameOverride string) string {
	t.Helper()
	dist := t.TempDir()
	checksums := make([]string, 0, len(releaseTargets)*2)
	for _, releaseTarget := range releaseTargets {
		name := "115driver_" + version + "_" + releaseTarget.goos + "_" + releaseTarget.archiveArch + ".tar.gz"
		pathName := filepath.Join(dist, name)
		if err := writeFixtureArchive(pathName, releaseTarget.goos, cliNameOverride); err != nil {
			t.Fatalf("write fixture %s: %v", name, err)
		}
		body, err := os.ReadFile(pathName)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		checksums = append(checksums, hex.EncodeToString(sum[:])+"  "+name)
		sbomName := name + ".spdx.json"
		if err := writeFixtureSBOM(filepath.Join(dist, sbomName), name); err != nil {
			t.Fatalf("write fixture SBOM %s: %v", name, err)
		}
		sbomBody, err := os.ReadFile(filepath.Join(dist, sbomName))
		if err != nil {
			t.Fatal(err)
		}
		sbomSum := sha256.Sum256(sbomBody)
		checksums = append(checksums, hex.EncodeToString(sbomSum[:])+"  "+sbomName)
	}
	sort.Strings(checksums)
	if err := os.WriteFile(filepath.Join(dist, "checksums.txt"), []byte(strings.Join(checksums, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dist
}

func rewriteChecksums(t *testing.T, dist string) {
	t.Helper()
	entries, err := os.ReadDir(dist)
	if err != nil {
		t.Fatal(err)
	}
	checksums := make([]string, 0, len(releaseTargets)*2)
	for _, entry := range entries {
		if entry.IsDir() || (!strings.HasSuffix(entry.Name(), ".tar.gz") && !strings.HasSuffix(entry.Name(), ".tar.gz.spdx.json")) {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dist, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(body)
		checksums = append(checksums, hex.EncodeToString(sum[:])+"  "+entry.Name())
	}
	sort.Strings(checksums)
	if err := os.WriteFile(filepath.Join(dist, "checksums.txt"), []byte(strings.Join(checksums, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeFixtureSBOM(pathName, archiveName string) error {
	body := `{"spdxVersion":"SPDX-2.3","dataLicense":"CC0-1.0","SPDXID":"SPDXRef-DOCUMENT","name":"` + archiveName + `","documentNamespace":"https://example.invalid/spdx/` + archiveName + `","creationInfo":{"creators":["Tool: fixture"]}}`
	return os.WriteFile(pathName, []byte(body), 0o600)
}

func writeFixtureArchive(pathName, goos, cliNameOverride string) error {
	file, err := os.Create(pathName)
	if err != nil {
		return err
	}
	defer file.Close()
	gz := gzip.NewWriter(file)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()

	exe := ""
	if goos == "windows" {
		exe = ".exe"
	}
	cliName := "115driver" + exe
	if cliNameOverride != "" {
		cliName = cliNameOverride
	}
	members := map[string]string{
		cliName:                      "cli-binary",
		"115driver-mcp-server" + exe: "mcp-binary",
		"LICENSE":                    "license",
		"README.md":                  "readme",
	}
	for name, body := range members {
		header := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			return err
		}
	}
	return nil
}
