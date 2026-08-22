package resolver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
)

func TestResolverDirectoryListingDisablesRecordOpenTime(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve resolver listing contract path")
	}
	path := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", "..", "internal", "remoteresolver", "resolver.go"))
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse shared remote resolver: %v", err)
	}

	found := 0
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "ListPage" {
			return true
		}
		found++
		if !resolverCallDisablesRemoteOpenTime(call) {
			position := fset.Position(call.Pos())
			t.Errorf("internal/remoteresolver/resolver.go:%d ListPage must use driver.WithRecordOpenTime(false)", position.Line)
		}
		return true
	})
	if found == 0 {
		t.Fatal("shared remote resolver contains no ListPage call to protect")
	}
}

func resolverCallDisablesRemoteOpenTime(call *ast.CallExpr) bool {
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
		value, valueOK := optionCall.Args[0].(*ast.Ident)
		if ok && pkg.Name == "driver" && valueOK && value.Name == "false" {
			return true
		}
	}
	return false
}
