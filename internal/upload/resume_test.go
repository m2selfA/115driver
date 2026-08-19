package upload

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/aliyun/aliyun-oss-go-sdk/oss"
)

func TestUploadResumeStateRoundTripAndIdentityReset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upload.session.json")
	state := newPreparedUploadResume("42", "file.bin", 123, "ABCDEF")
	params := driver.UploadOSSParams{SHA1: "ABCDEF", Bucket: "bucket", Object: "object"}
	params.Callback.Callback = `{"callbackBody":"sha1=${sha1}"}`
	params.Callback.CallbackVar = `{"x:dir":"U_1_42"}`
	state.setMultipart("https://oss.example", params, "upload-id", 32<<20, false)
	if err := saveUploadResume(path, state); err != nil {
		t.Fatal(err)
	}

	got, err := loadUploadResume(path, "42", "file.bin", 123, "abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.UploadID != "upload-id" || got.Params.Bucket != "bucket" || got.ChunkSize != 32<<20 {
		t.Fatalf("unexpected state: %#v", got)
	}
	if strings.Contains(string(mustReadUploadResume(t, path)), "AccessKey") || strings.Contains(string(mustReadUploadResume(t, path)), "SecurityToken") {
		t.Fatal("resume state unexpectedly contains STS credentials")
	}

	got, err = loadUploadResume(path, "42", "file.bin", 124, "ABCDEF")
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("changed local identity reused stale state: %#v", got)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale state was not removed: %v", err)
	}
}

func TestUploadResumeStateRejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	_, err := loadUploadResume(link, "1", "a", 1, "A")
	if !errors.Is(err, ErrUploadResumeState) {
		t.Fatalf("expected resume symlink rejection, got %v", err)
	}
}

func TestValidateSequentialResumePartsRequiresContiguousPrefix(t *testing.T) {
	if err := validateSequentialResumeParts([]oss.UploadPart{{PartNumber: 1}, {PartNumber: 2}}); err != nil {
		t.Fatalf("contiguous prefix should resume: %v", err)
	}
	if err := validateSequentialResumeParts([]oss.UploadPart{{PartNumber: 1}, {PartNumber: 3}}); !errors.Is(err, ErrUploadResumeState) {
		t.Fatalf("expected gap rejection, got %v", err)
	}
}

func mustReadUploadResume(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
