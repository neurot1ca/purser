package enforce

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"strings"
)

func extractCodeMapGo(source []byte) ([]codeMapSymbol, error) {
	fset := token.NewFileSet()
	file, _ := parser.ParseFile(fset, "", source, parser.AllErrors)
	if file == nil {
		return nil, nil
	}
	symbols := []codeMapSymbol{}
	for _, declaration := range file.Decls {
		switch decl := declaration.(type) {
		case *ast.FuncDecl:
			var signature bytes.Buffer
			copy := *decl
			copy.Body = nil
			if format.Node(&signature, fset, &copy) != nil {
				continue
			}
			name := decl.Name.Name
			kind := "function"
			if decl.Recv != nil && len(decl.Recv.List) > 0 {
				name = goReceiverName(decl.Recv.List[0].Type) + "." + name
				kind = "method"
			}
			symbols = append(symbols, codeMapSymbol{Line: fset.Position(decl.Pos()).Line, Kind: kind, Name: name, Signature: oneLineGo(signature.String())})
		case *ast.GenDecl:
			if decl.Tok != token.TYPE {
				continue
			}
			for _, spec := range decl.Specs {
				typeSpec, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				var signature bytes.Buffer
				if format.Node(&signature, fset, typeSpec) != nil {
					continue
				}
				symbols = append(symbols, codeMapSymbol{Line: fset.Position(typeSpec.Pos()).Line, Kind: "type", Name: typeSpec.Name.Name, Signature: oneLineGo("type " + signature.String())})
			}
		}
	}
	return symbols, nil
}

func goReceiverName(expr ast.Expr) string {
	switch receiver := expr.(type) {
	case *ast.StarExpr:
		return goReceiverName(receiver.X)
	case *ast.IndexExpr:
		return goReceiverName(receiver.X)
	case *ast.IndexListExpr:
		return goReceiverName(receiver.X)
	case *ast.ParenExpr:
		return goReceiverName(receiver.X)
	case *ast.Ident:
		return receiver.Name
	default:
		var name bytes.Buffer
		if format.Node(&name, token.NewFileSet(), expr) == nil {
			return strings.TrimLeft(name.String(), "*() ")
		}
		return "unknown"
	}
}

func oneLineGo(signature string) string {
	return strings.Join(strings.Fields(signature), " ")
}
