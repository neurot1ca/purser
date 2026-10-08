package enforce

import (
	"errors"
	"sort"
	"strings"

	"github.com/odvcencio/gotreesitter"
	"github.com/odvcencio/gotreesitter/grammars"
)

func extractPythonSymbols(source []byte) ([]codeMapSymbol, error) {
	lang := grammars.PythonLanguage()
	if lang == nil {
		return nil, errors.New("Python grammar unavailable")
	}
	tree, err := gotreesitter.NewParser(lang).Parse(source)
	if err != nil {
		return nil, err
	}
	if tree.RootNode().HasError() {
		return nil, errors.New("invalid Python syntax")
	}
	var symbols []codeMapSymbol
	var walk func(*gotreesitter.Node, string, bool)
	walk = func(node *gotreesitter.Node, scope string, classBody bool) {
		if node == nil {
			return
		}
		typ := node.Type(lang)
		if typ == "function_definition" || typ == "class_definition" {
			nameNode := node.ChildByFieldName("name", lang)
			if nameNode == nil {
				return
			}
			name := string(source[nameNode.StartByte():nameNode.EndByte()])
			qualified := name
			if scope != "" {
				qualified = scope + "." + name
			}
			kind := "class"
			signature := ""
			if typ == "function_definition" {
				kind = "function"
				if classBody {
					kind = "method"
				}
				if params := node.ChildByFieldName("parameters", lang); params != nil {
					signature = canonicalPythonSignature(string(source[params.StartByte():params.EndByte()]))
				}
				if result := node.ChildByFieldName("return_type", lang); result != nil {
					signature += " -> " + canonicalPythonSignature(string(source[result.StartByte():result.EndByte()]))
				}
			}
			symbols = append(symbols, codeMapSymbol{Line: int(node.StartPoint().Row) + 1, Kind: kind, Name: qualified, Signature: signature})
			if body := node.ChildByFieldName("body", lang); body != nil {
				walk(body, qualified, typ == "class_definition")
			}
			return
		}
		for i := 0; i < node.ChildCount(); i++ {
			walk(node.Child(i), scope, classBody)
		}
	}
	walk(tree.RootNode(), "", false)
	sort.Slice(symbols, func(i, j int) bool {
		if symbols[i].Line != symbols[j].Line {
			return symbols[i].Line < symbols[j].Line
		}
		return symbols[i].Name < symbols[j].Name
	})
	return symbols, nil
}

func canonicalPythonSignature(text string) string {
	out := make([]byte, 0, len(text))
	quote := byte(0)
	escaped := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if quote != 0 {
			if c == quote && !escaped {
				if quote == '"' {
					c = '\''
				}
				quote = 0
			}
			out = append(out, c)
			escaped = c == '\\' && !escaped
			if c != '\\' {
				escaped = false
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			if c == '"' {
				c = '\''
			}
			out = append(out, c)
			continue
		}
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' {
			if len(out) > 0 && out[len(out)-1] != ' ' {
				out = append(out, ' ')
			}
			continue
		}
		if c == '=' && (i == 0 || text[i-1] != '=' && text[i-1] != '!' && text[i-1] != '<' && text[i-1] != '>') && (i+1 == len(text) || text[i+1] != '=') {
			for len(out) > 0 && (out[len(out)-1] == ' ' || out[len(out)-1] == '\t') {
				out = out[:len(out)-1]
			}
			out = append(out, '=')
			for i+1 < len(text) && (text[i+1] == ' ' || text[i+1] == '\t') {
				i++
			}
			continue
		}
		out = append(out, c)
	}
	signature := strings.TrimSpace(string(out))
	runes := []rune(signature)
	if len(runes) > 160 {
		return string(runes[:159]) + "…"
	}
	return signature
}
