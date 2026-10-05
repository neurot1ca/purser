package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

func TestCodexResultOperationDoesNotStartFinalizer(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(".", "private.go"), nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok {
			continue
		}
		for _, forbidden := range []string{"operationMain", "privateMain"} {
			if function.Name.Name != forbidden {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if identifier, ok := call.Fun.(*ast.Ident); ok && (identifier.Name == "launchFinalizer" || identifier.Name == "finalizeMain") {
					t.Errorf("%s starts detached PostToolUse finalization", function.Name.Name)
				}
				return true
			})
		}
	}
}
