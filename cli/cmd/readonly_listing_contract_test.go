package cmd

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func callDisablesRemoteOpenTime(call *ast.CallExpr) bool {
	for _, arg := range call.Args {
		optionCall, ok := arg.(*ast.CallExpr)
		if !ok || len(optionCall.Args) != 1 {
			continue
		}
		selector, ok := optionCall.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "WithRecordOpenTime" {
			continue
		}
		pkg, ok := selector.X.(*ast.Ident)
		if !ok || pkg.Name != "driver" {
			continue
		}
		value, ok := optionCall.Args[0].(*ast.Ident)
		if ok && value.Name == "false" {
			return true
		}
	}
	return false
}

func TestProductionRemoteDirectoryReadsDisableRecordOpenTime(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve readonly listing contract path")
	}
	dir := filepath.Dir(currentFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	var violations []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", entry.Name(), err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			selector, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (selector.Sel.Name != "List" && selector.Sel.Name != "ListPage") {
				return true
			}
			// sync journal Store.List is local filesystem state, not a 115 API read.
			if receiver, ok := selector.X.(*ast.Ident); ok && receiver.Name == "store" && selector.Sel.Name == "List" {
				return true
			}
			if !callDisablesRemoteOpenTime(call) {
				position := fset.Position(call.Pos())
				violations = append(violations, fmt.Sprintf("%s:%d %s", entry.Name(), position.Line, selector.Sel.Name))
			}
			return true
		})
	}
	if len(violations) != 0 {
		t.Fatalf("production remote directory reads must use driver.WithRecordOpenTime(false): %s", strings.Join(violations, ", "))
	}
}
