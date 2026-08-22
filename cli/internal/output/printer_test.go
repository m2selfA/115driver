package output

import (
	"encoding/json"
	"io"
	"os"
	"testing"
)

func TestPrintErrorDataJSONIncludesStructuredData(t *testing.T) {
	oldStdout := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = oldStdout })

	printer := NewPrinter(true)
	if code := printer.PrintErrorData("batch incomplete", ExitError, map[string]interface{}{"failed": 1, "remaining": 2}); code != ExitError {
		t.Fatalf("unexpected exit code: %d", code)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	encoded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Success bool                   `json:"success"`
		Data    map[string]interface{} `json:"data"`
		Error   string                 `json:"error"`
		Code    int                    `json:"code"`
	}
	if err := json.Unmarshal(encoded, &envelope); err != nil {
		t.Fatalf("decode JSON envelope: %v\n%s", err, encoded)
	}
	if envelope.Success || envelope.Error != "batch incomplete" || envelope.Code != ExitError {
		t.Fatalf("unexpected error envelope: %#v", envelope)
	}
	if envelope.Data["failed"] != float64(1) || envelope.Data["remaining"] != float64(2) {
		t.Fatalf("structured error data missing: %#v", envelope.Data)
	}
}
