package driver

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

var destructiveLiveAPIMethods = map[string]struct{}{
	"AddOfflineTaskURIs":       {},
	"CleanRecycleBin":          {},
	"ClearOfflineTasks":        {},
	"Copy":                     {},
	"Delete":                   {},
	"DeleteOfflineTasks":       {},
	"LoginCheck":               {},
	"Mkdir":                    {},
	"Move":                     {},
	"RapidUpload":              {},
	"RapidUploadOrByMultipart": {},
	"RapidUploadOrByOSS":       {},
	"Rename":                   {},
	"RevertRecycleBin":         {},
	"UploadByMultipart":        {},
	"UploadByOSS":              {},
	"UploadFastOrByMultipart":  {},
	"UploadFastOrByOSS":        {},
	"UploadSHA1":               {},
}

var liveListAPIMethods = map[string]struct{}{
	"List":          {},
	"ListPage":      {},
	"ListWithLimit": {},
}

func callDisablesRecordOpenTime(call *ast.CallExpr) bool {
	for _, arg := range call.Args {
		optionCall, ok := arg.(*ast.CallExpr)
		if !ok || len(optionCall.Args) != 1 {
			continue
		}
		name, ok := optionCall.Fun.(*ast.Ident)
		if !ok || name.Name != "WithRecordOpenTime" {
			continue
		}
		value, ok := optionCall.Args[0].(*ast.Ident)
		if ok && value.Name == "false" {
			return true
		}
	}
	return false
}

func isOfflineLoginCheck(target *ast.SelectorExpr) bool {
	if target.Sel.Name != "LoginCheck" {
		return false
	}
	receiver, ok := target.X.(*ast.CallExpr)
	if !ok || len(receiver.Args) != 0 {
		return false
	}
	factory, ok := receiver.Fun.(*ast.Ident)
	return ok && factory.Name == "newOfflineStatusClient"
}

func TestLiveIntegrationMutationCallsRequireDestructiveGate(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve integration gate contract test path")
	}

	paths, err := filepath.Glob(filepath.Join(filepath.Dir(currentFile), "*_test.go"))
	if err != nil {
		t.Fatalf("glob driver tests: %v", err)
	}
	if len(paths) == 0 {
		t.Fatal("no driver test files found")
	}

	for _, path := range paths {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", filepath.Base(path), err)
		}

		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil || function.Body == nil || !strings.HasPrefix(function.Name.Name, "Test") {
				continue
			}

			hasLiveGate := false
			hasDestructiveGate := false
			mutations := map[string]struct{}{}
			unsafeLiveLists := map[string]struct{}{}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}

				switch target := call.Fun.(type) {
				case *ast.Ident:
					switch target.Name {
					case "teardown", "require115Integration":
						hasLiveGate = true
					case "destructiveTeardown", "require115DestructiveIntegration":
						hasLiveGate = true
						hasDestructiveGate = true
					}
				case *ast.SelectorExpr:
					if _, destructive := destructiveLiveAPIMethods[target.Sel.Name]; destructive && !isOfflineLoginCheck(target) {
						mutations[target.Sel.Name] = struct{}{}
					}
					if _, listAPI := liveListAPIMethods[target.Sel.Name]; listAPI && !callDisablesRecordOpenTime(call) {
						unsafeLiveLists[target.Sel.Name] = struct{}{}
					}
				}
				return true
			})

			isLegacyLiveFile := filepath.Base(path) == "driver_test.go"
			if len(unsafeLiveLists) > 0 && (isLegacyLiveFile || hasLiveGate) {
				methods := make([]string, 0, len(unsafeLiveLists))
				for method := range unsafeLiveLists {
					methods = append(methods, method)
				}
				sort.Strings(methods)
				t.Errorf("%s: %s calls live list API(s) %s without WithRecordOpenTime(false)", filepath.Base(path), function.Name.Name, strings.Join(methods, ", "))
			}

			if len(mutations) == 0 {
				continue
			}
			// driver_test.go is the legacy live-integration file. Other test files
			// are checked only when the test explicitly enters the live gate, so
			// deterministic mock/unit tests remain free to exercise mutation APIs.
			if !isLegacyLiveFile && !hasLiveGate {
				continue
			}
			if hasDestructiveGate {
				continue
			}

			methods := make([]string, 0, len(mutations))
			for method := range mutations {
				methods = append(methods, method)
			}
			sort.Strings(methods)
			t.Errorf("%s: %s calls destructive live API(s) %s without destructive integration gate", filepath.Base(path), function.Name.Name, strings.Join(methods, ", "))
		}
	}
}
