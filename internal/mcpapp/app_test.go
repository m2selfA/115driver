package mcpapp

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunVersionDoesNotRequireAuthentication(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("Run(--version) exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), ProgramName+" ") {
		t.Fatalf("Run(--version) output = %q, want %q prefix", stdout.String(), ProgramName+" ")
	}
}

func TestRunHelpDoesNotRequireAuthentication(t *testing.T) {
	for _, arg := range []string{"--help", "-h"} {
		t.Run(arg, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := Run([]string{arg}, &stdout, &stderr); code != 0 {
				t.Fatalf("Run(%s) exit = %d, stderr = %q", arg, code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "Usage: "+ProgramName+" [OPTIONS]") {
				t.Fatalf("Run(%s) stderr = %q", arg, stderr.String())
			}
			if !strings.Contains(stderr.String(), "maximum bytes per file for all MCP download tools") {
				t.Fatalf("Run(%s) help did not describe the shared per-file download size limit: %q", arg, stderr.String())
			}
			if !strings.Contains(stderr.String(), "allow-sensitive-tools") || !strings.Contains(stderr.String(), "signed URLs") {
				t.Fatalf("Run(%s) help did not describe sensitive MCP opt-in: %q", arg, stderr.String())
			}
		})
	}
}

func TestRunRejectsInvalidLocalRootBeforeAuthentication(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--local-root=" + missing}, &stdout, &stderr); code != 2 {
		t.Fatalf("Run(invalid local root) exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Invalid local root") {
		t.Fatalf("Run(invalid local root) stderr = %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "Cookie is required") || strings.Contains(stderr.String(), "Authentication failed") {
		t.Fatalf("invalid local root was validated after authentication setup: %q", stderr.String())
	}
}

func TestRunRejectsInvalidOptionsBeforeAuthentication(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"--url-upload-max-bytes=-1"}, &stdout, &stderr); code != 2 {
		t.Fatalf("Run(invalid options) exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "url-upload-max-bytes must be >= 0") {
		t.Fatalf("Run(invalid options) stderr = %q", stderr.String())
	}
}

func TestRunRejectsPositionalArgumentsBeforeAuthentication(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"unexpected"}, &stdout, &stderr); code != 2 {
		t.Fatalf("Run(positional arg) exit = %d, stderr = %q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "Unexpected positional arguments") {
		t.Fatalf("Run(positional arg) stderr = %q", stderr.String())
	}
}

func TestRunUsesReadOnlyCookieCheck(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve MCP app contract test path")
	}
	appPath := filepath.Join(filepath.Dir(currentFile), "app.go")
	file, err := parser.ParseFile(token.NewFileSet(), appPath, nil, 0)
	if err != nil {
		t.Fatalf("parse app.go: %v", err)
	}

	var run *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "Run" {
			run = fn
			break
		}
	}
	if run == nil || run.Body == nil {
		t.Fatal("Run function not found")
	}

	usesCookieCheck := false
	usesLoginCheck := false
	ast.Inspect(run.Body, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch selector.Sel.Name {
		case "CookieCheck":
			usesCookieCheck = true
		case "LoginCheck":
			usesLoginCheck = true
		}
		return true
	})
	if usesLoginCheck {
		t.Fatal("MCP startup must not call LoginCheck: it can log out other devices")
	}
	if !usesCookieCheck {
		t.Fatal("MCP startup must validate credentials with read-only CookieCheck")
	}
}
