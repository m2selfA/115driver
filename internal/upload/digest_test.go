package upload

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestPrepareFileDigestHashesAndRewinds(t *testing.T) {
	path := t.TempDir() + string(os.PathSeparator) + "payload.bin"
	if err := os.WriteFile(path, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	digest, err := PrepareFileDigest(file, 7)
	if err != nil {
		t.Fatal(err)
	}
	if digest.Size != 7 || !strings.EqualFold(digest.SHA1, "F07E5A815613C5ABEDDC4B682247A4C42D8A95DF") {
		t.Fatalf("unexpected digest: %#v", digest)
	}
	buf := make([]byte, 7)
	if _, err := file.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "payload" {
		t.Fatalf("file was not rewound after digest: %q", buf)
	}
}

func TestResolveUploadDigestReusesPreparedIdentity(t *testing.T) {
	path := t.TempDir() + string(os.PathSeparator) + "payload.bin"
	if err := os.WriteFile(path, []byte("aaaa"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	prepared, err := PrepareFileDigest(file, 4)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveUploadDigest(file, 4, prepared)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.SHA1 != prepared.SHA1 || resolved.PreID != prepared.PreID {
		t.Fatalf("prepared digest was recomputed: prepared=%#v resolved=%#v", prepared, resolved)
	}
}

func TestResolveUploadDigestRejectsChangedFile(t *testing.T) {
	path := t.TempDir() + string(os.PathSeparator) + "payload.bin"
	if err := os.WriteFile(path, []byte("aaaa"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	prepared, err := PrepareFileDigest(file, 4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt([]byte("zzzz"), 0); err != nil {
		t.Fatal(err)
	}
	changed := time.Unix(0, prepared.ModTimeUnixNano).Add(2 * time.Second)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveUploadDigest(file, 4, prepared); err == nil || !strings.Contains(err.Error(), "changed after digest preparation") {
		t.Fatalf("expected changed-file rejection, got %v", err)
	}
}
