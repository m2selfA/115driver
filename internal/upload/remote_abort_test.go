package upload

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
)

func TestLoadMultipartAbortStateOnlyActivatesCompleteMultipart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resume.json")
	state := newPreparedUploadResume("42", "file.bin", 123, "ABCDEF")
	if err := saveUploadResume(path, state); err != nil {
		t.Fatal(err)
	}
	if _, active, err := loadMultipartAbortState(path); err != nil || active {
		t.Fatalf("prepared state unexpectedly requested remote abort: active=%v err=%v", active, err)
	}
	if active, err := InspectRemoteResumeMultipart(path); err != nil || active {
		t.Fatalf("exported inspection unexpectedly activated prepared state: active=%v err=%v", active, err)
	}

	params := driver.UploadOSSParams{SHA1: "ABCDEF", Bucket: "bucket", Object: "object"}
	state.setMultipart("https://oss.example.invalid", params, "upload-id", 32<<20, false)
	if err := saveUploadResume(path, state); err != nil {
		t.Fatal(err)
	}
	loaded, active, err := loadMultipartAbortState(path)
	if err != nil || !active {
		t.Fatalf("complete multipart state was not activated: active=%v err=%v", active, err)
	}
	if loaded.UploadID != "upload-id" || loaded.Params.Bucket != "bucket" || loaded.Params.Object != "object" {
		t.Fatalf("unexpected abort state: %#v", loaded)
	}
	if active, err := InspectRemoteResumeMultipart(path); err != nil || !active {
		t.Fatalf("exported inspection did not detect multipart state: active=%v err=%v", active, err)
	}

	state.UploadID = ""
	encoded := mustReadUploadResume(t, path)
	_ = encoded
	if err := saveUploadResume(path, state); err != nil {
		t.Fatal(err)
	}
	if _, active, err := loadMultipartAbortState(path); err == nil || !active {
		t.Fatalf("incomplete multipart state should fail closed: active=%v err=%v", active, err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("abort state inspection mutated local resume state: %v", err)
	}
}
