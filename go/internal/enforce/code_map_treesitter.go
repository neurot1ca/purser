package enforce

import (
	"errors"
	"sort"
	"strings"

	"github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars/bash"
	"github.com/odvcencio/gotreesitter/grammars/c"
	"github.com/odvcencio/gotreesitter/grammars/c_sharp"
	"github.com/odvcencio/gotreesitter/grammars/cpp"
	"github.com/odvcencio/gotreesitter/grammars/java"
	"github.com/odvcencio/gotreesitter/grammars/javascript"
	"github.com/odvcencio/gotreesitter/grammars/kotlin"
	"github.com/odvcencio/gotreesitter/grammars/lua"
	"github.com/odvcencio/gotreesitter/grammars/php"
	"github.com/odvcencio/gotreesitter/grammars/ruby"
	"github.com/odvcencio/gotreesitter/grammars/rust"
	"github.com/odvcencio/gotreesitter/grammars/scala"
	"github.com/odvcencio/gotreesitter/grammars/swift"
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
	case "rust":
		grammar = rust.Language()
	case "java":
		grammar = java.Language()
	case "c":
		grammar = c.Language()
	case "cpp":
		grammar = cpp.Language()
	case "ruby":
		grammar = ruby.Language()
	case "php":
		grammar = php.Language()
	case "swift":
		grammar = swift.Language()
	case "kotlin":
		grammar = kotlin.Language()
	case "scala":
		grammar = scala.Language()
	case "lua":
		grammar = lua.Language()
	case "bash":
		grammar = bash.Language()
	case "c_sharp":
		grammar = c_sharp.Language()
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
	if language != "javascript" && language != "typescript" && language != "tsx" {
		return extractTreeSitterTagSymbols(source, grammar, language, tree.RootNode())
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

func extractTreeSitterTagSymbols(source []byte, grammar *gotreesitter.Language, language string, root *gotreesitter.Node) ([]codeMapSymbol, error) {
	type definition struct {
		node, name *gotreesitter.Node
		kind       string
	}
	var definitions []definition
	var walk func(*gotreesitter.Node)
	walk = func(node *gotreesitter.Node) {
		if node == nil {
			return
		}
		typ := node.Type(grammar)
		nameNode := node.ChildByFieldName("name", grammar)
		add := func(kind string, name *gotreesitter.Node) {
			if name != nil {
				definitions = append(definitions, definition{node: node, name: name, kind: kind})
			}
		}
		field := func(parent *gotreesitter.Node, field, childType string) *gotreesitter.Node {
			child := parent.ChildByFieldName(field, grammar)
			if child != nil && child.Type(grammar) == childType {
				return child
			}
			return nil
		}
		switch language {
		case "rust":
			switch typ {
			case "struct_item", "enum_item", "union_item", "type_item":
				add("class", field(node, "name", "type_identifier"))
			case "trait_item":
				add("interface", field(node, "name", "type_identifier"))
			case "function_item":
				name := field(node, "name", "identifier")
				add("function", name)
				if parent := node.Parent(); parent != nil && parent.Type(grammar) == "declaration_list" {
					add("method", name)
				}
			}
		case "java":
			switch typ {
			case "class_declaration":
				add("class", field(node, "name", "identifier"))
			case "interface_declaration":
				add("interface", field(node, "name", "identifier"))
			case "method_declaration", "constructor_declaration":
				add("method", field(node, "name", "identifier"))
			}
		case "c":
			switch typ {
			case "struct_specifier":
				if field(node, "body", "field_declaration_list") != nil {
					add("class", field(node, "name", "type_identifier"))
				}
			case "union_specifier":
				if parent := node.Parent(); parent != nil && parent.Type(grammar) == "declaration" && parent.ChildByFieldName("type", grammar) == node {
					add("class", field(node, "name", "type_identifier"))
				}
			case "function_declarator":
				add("function", field(node, "declarator", "identifier"))
			case "type_definition":
				add("type", field(node, "declarator", "type_identifier"))
			case "enum_specifier":
				add("type", field(node, "name", "type_identifier"))
			}
		case "cpp":
			switch typ {
			case "class_specifier":
				add("class", field(node, "name", "type_identifier"))
			case "function_definition":
				declarator := field(node, "declarator", "function_declarator")
				if declarator != nil {
					add("function", field(declarator, "declarator", "identifier"))
				}
			}
		case "ruby":
			switch typ {
			case "class":
				add("class", nameNode)
			case "method", "singleton_method":
				add("method", field(node, "name", "identifier"))
			}
		case "php":
			switch typ {
			case "class_declaration":
				add("class", field(node, "name", "name"))
			case "interface_declaration":
				add("interface", field(node, "name", "name"))
			case "method_declaration":
				add("method", field(node, "name", "name"))
			case "function_definition":
				add("function", field(node, "name", "name"))
			}
		case "swift":
			switch typ {
			case "class_declaration":
				add("class", field(node, "name", "type_identifier"))
				// The Python tags query captures the enclosing declaration as a method
				// when it contains a method declaration.
				if name := field(node, "name", "type_identifier"); name != nil && hasDescendantType(node, grammar, "function_declaration") {
					add("method", name)
				}
			case "protocol_declaration":
				add("interface", field(node, "name", "type_identifier"))
				if name := field(node, "name", "type_identifier"); name != nil {
					add("method", name)
				}
			case "function_declaration":
				add("function", field(node, "name", "simple_identifier"))
			case "init_declaration":
				add("method", field(node, "name", "simple_identifier"))
			case "deinit_declaration":
				add("method", field(node, "name", "simple_identifier"))
			}
		case "kotlin":
			switch typ {
			case "class_declaration":
				add("class", firstDescendantType(node, grammar, "type_identifier"))
			case "object_declaration", "companion_object":
				add("class", firstDescendantType(node, grammar, "type_identifier"))
			case "function_declaration":
				add("function", firstDescendantType(node, grammar, "simple_identifier"))
			case "type_alias":
				add("type", firstDescendantType(node, grammar, "type_identifier"))
			}
		case "scala":
			switch typ {
			case "class_definition":
				add("class", field(node, "name", "identifier"))
			case "trait_definition":
				add("interface", field(node, "name", "identifier"))
			case "function_definition":
				add("method", field(node, "name", "identifier"))
			}
		case "lua":
			switch typ {
			case "function_declaration":
				name := field(node, "name", "identifier")
				if name == nil {
					name = lastDescendantType(node.ChildByFieldName("name", grammar), grammar, "identifier")
				}
				kind := "function"
				if named := node.ChildByFieldName("name", grammar); named != nil && named.Type(grammar) == "method_index_expression" {
					kind = "method"
				}
				add(kind, name)
			case "assignment_statement":
				if values := firstDescendantType(node, grammar, "expression_list"); values != nil && values.NamedChildCount() > 0 && values.NamedChild(0).Type(grammar) == "function_definition" {
					if names := firstDescendantType(node, grammar, "variable_list"); names != nil {
						add("function", lastDescendantType(names, grammar, "identifier"))
					}
				}
			case "field":
				if field(node, "value", "function_definition") != nil {
					add("function", field(node, "name", "identifier"))
				}
			}
		case "bash":
			if typ == "function_definition" {
				add("function", field(node, "name", "word"))
			}
		case "c_sharp":
			switch typ {
			case "class_declaration":
				add("class", field(node, "name", "identifier"))
			case "interface_declaration":
				add("interface", field(node, "name", "identifier"))
			case "struct_declaration":
				add("struct", field(node, "name", "identifier"))
			case "enum_declaration":
				add("enum", field(node, "name", "identifier"))
			case "method_declaration", "constructor_declaration", "local_function_statement":
				add("method", field(node, "name", "identifier"))
			}
		}
		for i := 0; i < node.NamedChildCount(); i++ {
			walk(node.NamedChild(i))
		}
	}
	walk(root)
	var symbols []codeMapSymbol
	type symbolKey struct {
		line       int
		kind, name string
	}
	seen := make(map[symbolKey]bool)
	for _, item := range definitions {
		name := strings.TrimSpace(string(source[item.name.StartByte():item.name.EndByte()]))
		if name == "" || strings.ContainsRune(name, '\n') {
			continue
		}
		var parents []string
		for parent := item.node.Parent(); parent != nil; parent = parent.Parent() {
			for _, candidate := range definitions {
				if candidate.node != parent || candidate.name == nil || !treeSitterScopeKind(candidate.kind) {
					continue
				}
				part := strings.TrimSpace(string(source[candidate.name.StartByte():candidate.name.EndByte()]))
				if part != "" && !strings.ContainsRune(part, '\n') {
					parents = append([]string{part}, parents...)
				}
				break
			}
		}
		kind := item.kind
		if kind == "function" && len(parents) > 0 {
			kind = "method"
		}
		qualified := strings.Join(append(parents, name), ".")
		key := symbolKey{line: int(item.name.StartPoint().Row) + 1, kind: kind, name: qualified}
		if !seen[key] {
			seen[key] = true
			symbols = append(symbols, codeMapSymbol{Line: int(item.name.StartPoint().Row) + 1, Kind: kind, Name: qualified})
		}
	}
	sort.Slice(symbols, func(i, j int) bool {
		if symbols[i].Line != symbols[j].Line {
			return symbols[i].Line < symbols[j].Line
		}
		return symbols[i].Name < symbols[j].Name
	})
	return symbols, nil
}

func firstDescendantType(node *gotreesitter.Node, grammar *gotreesitter.Language, typ string) *gotreesitter.Node {
	if node == nil {
		return nil
	}
	if node.Type(grammar) == typ {
		return node
	}
	for i := 0; i < node.NamedChildCount(); i++ {
		if found := firstDescendantType(node.NamedChild(i), grammar, typ); found != nil {
			return found
		}
	}
	return nil
}

func lastDescendantType(node *gotreesitter.Node, grammar *gotreesitter.Language, typ string) *gotreesitter.Node {
	if node == nil {
		return nil
	}
	var found *gotreesitter.Node
	if node.Type(grammar) == typ {
		found = node
	}
	for i := 0; i < node.NamedChildCount(); i++ {
		if child := lastDescendantType(node.NamedChild(i), grammar, typ); child != nil {
			found = child
		}
	}
	return found
}

func hasDescendantType(node *gotreesitter.Node, grammar *gotreesitter.Language, typ string) bool {
	return firstDescendantType(node, grammar, typ) != nil
}

func treeSitterScopeKind(kind string) bool {
	switch kind {
	case "class", "struct", "interface", "trait", "enum":
		return true
	default:
		return false
	}
}
