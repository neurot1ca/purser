package proc

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestProductionCommandsAreHidden(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var violations []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == ".tmp" || entry.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			commands := map[string]token.Pos{}
			guarded := map[string]bool{}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if commandCall(call) {
					wrapped := false
					ast.Inspect(fn.Body, func(candidate ast.Node) bool {
						parent, ok := candidate.(*ast.CallExpr)
						if ok && isHidingCall(parent) && nodeWithin(call, parent) {
							wrapped = true
						}
						return true
					})
					if !wrapped {
						for _, stmt := range []ast.Node{fn.Body} {
							ast.Inspect(stmt, func(candidate ast.Node) bool {
								assignment, ok := candidate.(*ast.AssignStmt)
								if !ok || !nodeWithin(call, assignment) {
									return true
								}
								for _, lhs := range assignment.Lhs {
									if id, ok := lhs.(*ast.Ident); ok {
										commands[id.Name] = call.Pos()
									}
								}
								return true
							})
						}
					}
				}
				if isHidingCall(call) && len(call.Args) > 0 {
					if id, ok := call.Args[0].(*ast.Ident); ok {
						guarded[id.Name] = true
					}
				}
				return true
			})
			for name, pos := range commands {
				if !guarded[name] {
					position := fset.Position(pos)
					violations = append(violations, filepath.ToSlash(filepath.Join(filepath.Base(root), strings.TrimPrefix(path, root+string(filepath.Separator))))+":"+strconv.Itoa(position.Line))
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) > 0 {
		t.Fatalf("production subprocesses must be passed to proc.Hide or proc.Configure*: %s", strings.Join(violations, ", "))
	}
}

func commandCall(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || (selector.Sel.Name != "Command" && selector.Sel.Name != "CommandContext") {
		return false
	}
	packageName, ok := selector.X.(*ast.Ident)
	return ok && packageName.Name == "exec"
}

func isHidingCall(call *ast.CallExpr) bool {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		return fn.Name == "Hide"
	case *ast.SelectorExpr:
		return fn.Sel.Name == "Hide" || strings.HasPrefix(fn.Sel.Name, "Configure")
	default:
		return false
	}
}

func nodeWithin(target, parent ast.Node) bool {
	return parent.Pos() <= target.Pos() && target.End() <= parent.End()
}
