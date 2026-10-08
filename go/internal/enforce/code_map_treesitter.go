package enforce

import (
	"errors"
	"sort"
	"strings"

	"github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars/javascript"
	"github.com/odvcencio/gotreesitter/grammars/tsx"
	"github.com/odvcencio/gotreesitter/grammars/typescript"
)

func extractTreeSitterSymbols(source []byte, language string) ([]codeMapSymbol, error) {
	var grammar *gotreesitter.Language
	switch language {
	case "javascript":
		grammar = javascript.Language()
	case "typescript":
		grammar = typescript.Language()
	case "tsx":
		grammar = tsx.Language()
	default:
		return nil, errors.New("unsupported tree-sitter language")
	}
	if grammar == nil {
		return nil, errors.New("tree-sitter grammar unavailable")
	}
	tree, err := gotreesitter.NewParser(grammar).Parse(source)
	if err != nil {
		return nil, err
	}
	if tree.RootNode().HasError() {
		return nil, errors.New("invalid syntax")
	}
	var symbols []codeMapSymbol
	var walk func(*gotreesitter.Node, string)
	walk = func(node *gotreesitter.Node, scope string) {
		if node == nil {
			return
		}
		typ := node.Type(grammar)
		nameNode := node.ChildByFieldName("name", grammar)
		kind := ""
		switch typ {
		case "function_declaration":
			kind = "function"
		case "class_declaration":
			kind = "class"
		case "method_definition":
			kind = "method"
		case "variable_declarator":
			value := node.ChildByFieldName("value", grammar)
			if value != nil && (value.Type(grammar) == "arrow_function" || value.Type(grammar) == "function_expression") {
				kind = "function"
			}
		}
		qualified := scope
		if kind != "" && nameNode != nil {
			name := strings.TrimSpace(string(source[nameNode.StartByte():nameNode.EndByte()]))
			if name != "" && !strings.ContainsRune(name, '\n') {
				if kind == "function" && scope != "" {
					kind = "method"
				}
				if qualified != "" {
					qualified += "."
				}
				qualified += name
				symbols = append(symbols, codeMapSymbol{Line: int(nameNode.StartPoint().Row) + 1, Kind: kind, Name: qualified})
			}
		}
		childScope := scope
		if typ == "class_declaration" && qualified != "" {
			childScope = qualified
		}
		if typ == "export_statement" && strings.HasPrefix(strings.TrimSpace(string(source[node.StartByte():node.EndByte()])), "export default") {
			for i := 0; i < node.NamedChildCount(); i++ {
				child := node.NamedChild(i)
				if child.Type(grammar) == "function_expression" || child.Type(grammar) == "arrow_function" {
					symbols = append(symbols, codeMapSymbol{Line: int(child.StartPoint().Row) + 1, Kind: "function", Name: "default"})
				}
			}
		}
		for i := 0; i < node.NamedChildCount(); i++ {
			walk(node.NamedChild(i), childScope)
		}
	}
	walk(tree.RootNode(), "")
	sort.Slice(symbols, func(i, j int) bool {
		if symbols[i].Line != symbols[j].Line {
			return symbols[i].Line < symbols[j].Line
		}
		return symbols[i].Name < symbols[j].Name
	})
	return symbols, nil
}
