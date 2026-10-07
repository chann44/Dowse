// Package chunk splits source files into function-level chunks with tree-sitter.
package chunk

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/rust"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

type Chunk struct {
	Name      string // e.g. "Client.backoff"
	Kind      string // tree-sitter node type, or "file"
	Signature string // first line of the definition
	StartLine int    // 1-based, inclusive
	EndLine   int
	Content   string
}

// Containers (classes, impls) smaller than this are kept whole;
// larger ones are split into their methods.
const smallContainerLines = 60

type lang struct {
	grammar    *sitter.Language
	chunks     map[string]bool // node types that become chunks
	containers map[string]bool // chunk types whose members become chunks when large
}

var tsChunks = map[string]bool{
	"function_declaration": true, "generator_function_declaration": true,
	"class_declaration": true, "abstract_class_declaration": true, "method_definition": true,
	"interface_declaration": true, "type_alias_declaration": true, "enum_declaration": true,
	"lexical_declaration": true, // only when it binds a function; see isFuncBinding
}
var tsContainers = map[string]bool{"class_declaration": true, "abstract_class_declaration": true}

var langs = map[string]*lang{
	".go": {golang.GetLanguage(),
		set("function_declaration", "method_declaration", "type_declaration"), nil},
	".py": {python.GetLanguage(),
		set("function_definition", "class_definition", "decorated_definition"),
		set("class_definition")},
	".rs": {rust.GetLanguage(),
		set("function_item", "impl_item", "trait_item", "struct_item", "enum_item", "macro_definition"),
		set("impl_item", "trait_item")},
	".js":  {javascript.GetLanguage(), tsChunks, tsContainers},
	".jsx": {javascript.GetLanguage(), tsChunks, tsContainers},
	".mjs": {javascript.GetLanguage(), tsChunks, tsContainers},
	".cjs": {javascript.GetLanguage(), tsChunks, tsContainers},
	".ts":  {typescript.GetLanguage(), tsChunks, tsContainers},
	".mts": {typescript.GetLanguage(), tsChunks, tsContainers},
	".tsx": {tsx.GetLanguage(), tsChunks, tsContainers},
}

func set(xs ...string) map[string]bool {
	m := make(map[string]bool, len(xs))
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// Supported reports whether dowse can chunk files with this path's extension.
func Supported(path string) bool {
	_, ok := langs[strings.ToLower(filepath.Ext(path))]
	return ok
}

// File parses src and returns its chunks. Files with no recognised
// definitions (scripts, config-like modules) become a single "file" chunk.
func File(ctx context.Context, path string, src []byte) ([]Chunk, error) {
	l := langs[strings.ToLower(filepath.Ext(path))]
	if l == nil {
		return nil, nil
	}
	p := sitter.NewParser()
	defer p.Close()
	p.SetLanguage(l.grammar)
	tree, err := p.ParseCtx(ctx, nil, src)
	if err != nil {
		return nil, err
	}
	defer tree.Close()

	w := walker{l: l, src: src}
	w.walk(tree.RootNode(), "")
	if len(w.out) == 0 && len(strings.TrimSpace(string(src))) > 0 {
		lines := strings.Count(string(src), "\n") + 1
		w.out = append(w.out, Chunk{
			Name: filepath.Base(path), Kind: "file", Signature: firstLine(string(src)),
			StartLine: 1, EndLine: lines, Content: string(src),
		})
	}
	return w.out, nil
}

type walker struct {
	l   *lang
	src []byte
	out []Chunk
}

func (w *walker) walk(n *sitter.Node, prefix string) {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		c := n.NamedChild(i)
		t := c.Type()
		if !w.l.chunks[t] || (t == "lexical_declaration" && !w.isFuncBinding(c)) {
			w.walk(c, prefix)
			continue
		}
		name := w.name(c)
		if prefix != "" && name != "" {
			name = prefix + "." + name
		}
		lines := int(c.EndPoint().Row-c.StartPoint().Row) + 1
		def := c
		if t == "decorated_definition" {
			if d := c.ChildByFieldName("definition"); d != nil {
				def = d
			}
		}
		if w.l.containers[def.Type()] && lines > smallContainerLines && w.hasMembers(def) {
			w.walk(def, name)
			continue
		}
		start := w.docStart(c)
		w.out = append(w.out, Chunk{
			Name: name, Kind: t, Signature: firstLine(c.Content(w.src)),
			StartLine: int(start.StartPoint().Row) + 1, EndLine: int(c.EndPoint().Row) + 1,
			Content: string(w.src[start.StartByte():c.EndByte()]),
		})
	}
}

// docStart returns the first node of the comment block directly above n
// (no blank line in between), or n itself. Doc comments carry most of a
// function's meaning, so they belong in its chunk.
func (w *walker) docStart(n *sitter.Node) *sitter.Node {
	start := n
	for p := n.PrevNamedSibling(); p != nil && strings.Contains(p.Type(), "comment"); p = p.PrevNamedSibling() {
		if p.EndPoint().Row+1 < start.StartPoint().Row {
			break
		}
		start = p
	}
	return start
}

// hasMembers reports whether a large container has any chunkable members;
// if not (e.g. a huge struct-like class), it is kept whole.
func (w *walker) hasMembers(n *sitter.Node) bool {
	found := false
	var visit func(*sitter.Node)
	visit = func(n *sitter.Node) {
		for i := 0; i < int(n.NamedChildCount()) && !found; i++ {
			c := n.NamedChild(i)
			if w.l.chunks[c.Type()] {
				found = true
				return
			}
			visit(c)
		}
	}
	visit(n)
	return found
}

// isFuncBinding matches `const f = () => {}` and `const f = function() {}`.
func (w *walker) isFuncBinding(n *sitter.Node) bool {
	for i := 0; i < int(n.NamedChildCount()); i++ {
		d := n.NamedChild(i)
		if d.Type() != "variable_declarator" {
			continue
		}
		if v := d.ChildByFieldName("value"); v != nil {
			switch v.Type() {
			case "arrow_function", "function", "function_expression", "generator_function":
				return true
			}
		}
	}
	return false
}

func (w *walker) name(n *sitter.Node) string {
	switch n.Type() {
	case "decorated_definition":
		if d := n.ChildByFieldName("definition"); d != nil {
			return w.name(d)
		}
	case "method_declaration": // Go: include the receiver type
		name := w.text(n.ChildByFieldName("name"))
		if r := n.ChildByFieldName("receiver"); r != nil {
			recv := strings.Trim(w.text(r), "()")
			if f := strings.Fields(recv); len(f) > 0 {
				recv = strings.TrimLeft(f[len(f)-1], "*")
				if i := strings.IndexByte(recv, '['); i >= 0 {
					recv = recv[:i]
				}
				return recv + "." + name
			}
		}
		return name
	case "type_declaration": // Go: name lives on the type_spec
		for i := 0; i < int(n.NamedChildCount()); i++ {
			if s := n.NamedChild(i); s.Type() == "type_spec" || s.Type() == "type_alias" {
				return w.text(s.ChildByFieldName("name"))
			}
		}
	case "impl_item": // Rust: `impl Trait for Type` -> Type
		return w.text(n.ChildByFieldName("type"))
	case "lexical_declaration":
		for i := 0; i < int(n.NamedChildCount()); i++ {
			if d := n.NamedChild(i); d.Type() == "variable_declarator" {
				return w.text(d.ChildByFieldName("name"))
			}
		}
	}
	return w.text(n.ChildByFieldName("name"))
}

func (w *walker) text(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	return n.Content(w.src)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
