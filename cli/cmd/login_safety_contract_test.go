package cmd

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCookieLoginUsesReadOnlyCookieCheck(t *testing.T) {
	t.Helper()

	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve login safety contract test path")
	}
	loginPath := filepath.Join(filepath.Dir(currentFile), "login.go")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, loginPath, nil, 0)
	if err != nil {
		t.Fatalf("parse login.go: %v", err)
	}

	var loginWithCookie *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "loginWithCookie" {
			loginWithCookie = fn
			break
		}
	}
	if loginWithCookie == nil || loginWithCookie.Body == nil {
		t.Fatal("loginWithCookie function not found")
	}

	usesCookieCheck := false
	usesLoginCheck := false
	ast.Inspect(loginWithCookie.Body, func(node ast.Node) bool {
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
		t.Fatal("loginWithCookie must not call LoginCheck: it can log out other devices")
	}
	if !usesCookieCheck {
		t.Fatal("loginWithCookie must validate credentials with read-only CookieCheck")
	}
}
