package remotetree

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"runtime"
	"testing"

	"github.com/SheltonZhu/115driver/pkg/driver"
)

type dualModeTreeClient struct {
	listCalls int
	pageCalls int
	list      *[]driver.File
}

func (c *dualModeTreeClient) List(string, ...driver.ListOption) (*[]driver.File, error) {
	c.listCalls++
	return c.list, nil
}

func (c *dualModeTreeClient) ListPage(string, int64, int64, ...driver.ListOption) (*[]driver.File, error) {
	c.pageCalls++
	empty := []driver.File{}
	return &empty, nil
}

type pagedTreeClient struct {
	calls int
	page  *[]driver.File
}

func (c *pagedTreeClient) ListPage(string, int64, int64, ...driver.ListOption) (*[]driver.File, error) {
	c.calls++
	return c.page, nil
}

func TestWalkPreservesFullListSemanticsWhenClientAlsoSupportsPaging(t *testing.T) {
	empty := []driver.File{}
	client := &dualModeTreeClient{list: &empty}
	if _, err := Walk(client, "root", "/root", 0, func(Entry) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if client.listCalls != 1 || client.pageCalls != 0 {
		t.Fatalf("Walk calls: List=%d ListPage=%d, want 1/0", client.listCalls, client.pageCalls)
	}
}

func TestWalkPagedStopsBeforeFetchingNextFullPage(t *testing.T) {
	page := make([]driver.File, WalkPageLimit)
	for i := range page {
		page[i] = driver.File{FileID: "file", Name: "file.bin"}
	}
	client := &pagedTreeClient{page: &page}
	visited := 0
	result, err := WalkPaged(client, "root", "/root", 0, func(Entry) (bool, error) {
		visited++
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.StoppedEarly || visited != 1 || client.calls != 1 {
		t.Fatalf("WalkPaged early stop: result=%#v visited=%d calls=%d", result, visited, client.calls)
	}
}

func TestWalkPagedRejectsNilPageAsUnexpected(t *testing.T) {
	client := &pagedTreeClient{page: nil}
	_, err := WalkPaged(client, "root", "/root", 0, func(Entry) (bool, error) { return false, nil })
	if !errors.Is(err, driver.ErrUnexpected) {
		t.Fatalf("WalkPaged nil page = %v, want ErrUnexpected", err)
	}
	if client.calls != 1 {
		t.Fatalf("WalkPaged nil page calls=%d, want 1", client.calls)
	}
}

func TestWalkPagedSourceDisablesRecordOpenTime(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve tree source path")
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, currentFile[:len(currentFile)-len("tree_test.go")]+"tree.go", nil, 0)
	if err != nil {
		t.Fatal(err)
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
		if !callHasRecordOpenTimeFalse(call) {
			t.Errorf("tree.go:%d ListPage must use driver.WithRecordOpenTime(false)", fset.Position(call.Pos()).Line)
		}
		return true
	})
	if found == 0 {
		t.Fatal("tree.go contains no ListPage call to protect")
	}
}

func callHasRecordOpenTimeFalse(call *ast.CallExpr) bool {
	for _, arg := range call.Args {
		optionCall, ok := arg.(*ast.CallExpr)
		if !ok || len(optionCall.Args) != 1 {
			continue
		}
		selector, ok := optionCall.Fun.(*ast.SelectorExpr)
		if !ok || selector.Sel.Name != "WithRecordOpenTime" {
			continue
		}
		pkg, pkgOK := selector.X.(*ast.Ident)
		value, valueOK := optionCall.Args[0].(*ast.Ident)
		if pkgOK && pkg.Name == "driver" && valueOK && value.Name == "false" {
			return true
		}
	}
	return false
}
