package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/doc/comment"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/ast/astutil"
)

// parsedFile is a single file parsed with comments, used for the doc comments in hovers and for the outline.
type parsedFile struct {
	stamp  string
	fset   *token.FileSet
	file   *ast.File
	mapper *mapper
}

func (s *server) parse(name string) (*parsedFile, error) {
	stamp := ""
	if ver, ok := s.overlayVersion(name); ok {
		stamp = fmt.Sprintf("o%d", ver)
	} else if st, err := os.Stat(name); err == nil {
		stamp = fmt.Sprintf("%d %d", st.ModTime().UnixNano(), st.Size())
	}
	s.mu.Lock()
	pf := s.parsed[name]
	s.mu.Unlock()
	if pf != nil && pf.stamp == stamp {
		return pf, nil
	}
	src, err := s.readFile(name)
	if err != nil {
		return nil, err
	}
	fset := token.NewFileSet()
	f, _ := parser.ParseFile(fset, name, src, parser.ParseComments|parser.SkipObjectResolution)
	if f == nil {
		return nil, fmt.Errorf("cannot parse %s", name)
	}
	pf = &parsedFile{stamp: stamp, fset: fset, file: f, mapper: newMapper(src)}
	s.mu.Lock()
	if len(s.parsed) > 256 {
		s.parsed = map[string]*parsedFile{}
	}
	s.parsed[name] = pf
	s.mu.Unlock()
	return pf, nil
}

// target is the identifier under the cursor and the object it refers to.
type target struct {
	pkg   *fullPkg
	file  *ast.File
	ident *ast.Ident
	obj   types.Object
	// onImport reports whether the cursor is on an import declaration.
	onImport bool
}

func (s *server) targetAt(file string, pos lspPosition) (*target, error) {
	p, err := s.packageFor(file)
	if err != nil {
		return nil, err
	}
	f, m := p.files[file], p.mappers[file]
	if f == nil || m == nil {
		return nil, fmt.Errorf("%s is not in package %s", file, p.meta.ID)
	}
	tf := s.fset.File(f.FileStart)
	off := m.offset(pos.Line, pos.Character)
	// An identifier that ends right before the cursor is also a target.
	for _, o := range []int{off, off - 1} {
		if o < 0 || o > tf.Size() {
			continue
		}
		path, _ := astutil.PathEnclosingInterval(f, tf.Pos(o), tf.Pos(o))
		for _, n := range path {
			switch n := n.(type) {
			case *ast.Ident:
				if obj := p.info.ObjectOf(n); obj != nil {
					return &target{pkg: p, file: f, ident: n, obj: obj}, nil
				}
			case *ast.ImportSpec:
				obj := p.info.Implicits[n]
				if n.Name != nil {
					obj = p.info.Defs[n.Name]
				}
				if obj != nil {
					return &target{pkg: p, file: f, obj: obj, onImport: true}, nil
				}
			case ast.Stmt, ast.Decl:
				goto next
			}
		}
	next:
	}
	return nil, nil
}

// objectPos returns the file that declares obj and the byte offset of the declaration in it.
func (s *server) objectPos(obj types.Object) (string, *mapper, int, bool) {
	if obj.Pkg() == nil {
		return s.builtinPos(obj.Name())
	}
	if !obj.Pos().IsValid() {
		return "", nil, 0, false
	}
	tp := posToken(s.fset, obj.Pos())
	if !filepath.IsAbs(tp.Filename) {
		return "", nil, 0, false
	}
	pf, err := s.parse(tp.Filename)
	if err != nil {
		return "", nil, 0, false
	}
	off := pf.mapper.lineColOffset(tp.Line, tp.Column)
	return tp.Filename, pf.mapper, fixColumn(pf.mapper, tp.Line, off, obj.Name()), true
}

// fixColumn returns the offset of name in the same line when off does not point at the start of name.
// A position restored from type information saved on disk has the right line, but its column is always 1.
func fixColumn(m *mapper, line, off int, name string) int {
	if isWordAt(m.content, off, name) {
		return off
	}
	if line < 1 || line > len(m.lineStarts) {
		return off
	}
	start := m.lineStarts[line-1]
	end := len(m.content)
	if line < len(m.lineStarts) {
		end = m.lineStarts[line]
	}
	for i := start; i+len(name) <= end; i++ {
		if isWordAt(m.content, i, name) {
			return i
		}
	}
	return off
}

func isWordAt(content []byte, off int, name string) bool {
	if name == "" || off < 0 || off+len(name) > len(content) || string(content[off:off+len(name)]) != name {
		return false
	}
	if off > 0 && isIdentByte(content[off-1]) {
		return false
	}
	return off+len(name) == len(content) || !isIdentByte(content[off+len(name)])
}

func isIdentByte(b byte) bool {
	return b == '_' || b >= 0x80 || (b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

// builtinPos looks up the declaration of the built-in identifier name in the builtin package of GOROOT.
func (s *server) builtinPos(name string) (string, *mapper, int, bool) {
	s.mu.Lock()
	var env goEnv
	for _, v := range s.views {
		env = v.env
		if env.GOROOT != "" {
			break
		}
	}
	s.mu.Unlock()
	if env.GOROOT == "" {
		return "", nil, 0, false
	}
	file := filepath.Join(env.GOROOT, "src", "builtin", "builtin.go")
	pf, err := s.parse(file)
	if err != nil {
		return "", nil, 0, false
	}
	var found *ast.Ident
	for _, d := range pf.file.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Name.Name == name {
				found = d.Name
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					if spec.Name.Name == name {
						found = spec.Name
					}
				case *ast.ValueSpec:
					for _, n := range spec.Names {
						if n.Name == name {
							found = n
						}
					}
				}
			}
		}
	}
	if found == nil {
		return "", nil, 0, false
	}
	return file, pf.mapper, pf.fset.Position(found.Pos()).Offset, true
}

func (s *server) objectLocation(obj types.Object) []lspLocation {
	file, m, off, ok := s.objectPos(obj)
	if !ok {
		return nil
	}
	end := off
	if isWordAt(m.content, off, obj.Name()) {
		end = off + len(obj.Name())
	}
	return []lspLocation{{URI: pathToURI(file), Range: lspRange{Start: m.position(off), End: m.position(end)}}}
}

func (s *server) definition(file string, pos lspPosition) ([]lspLocation, error) {
	t, err := s.targetAt(file, pos)
	if t == nil || err != nil {
		return nil, err
	}
	if pn, ok := t.obj.(*types.PkgName); ok && t.onImport {
		// On an import declaration, go to a file of the imported package.
		return s.packageLocation(t.pkg.view, pn.Imported()), nil
	}
	return s.objectLocation(t.obj), nil
}

func (s *server) packageLocation(v *view, tp *types.Package) []lspLocation {
	m := v.metaOf(tp.Path())
	if m == nil || len(m.GoFiles) == 0 {
		return nil
	}
	best := m.GoFiles[0]
	for _, f := range m.GoFiles {
		if base := filepath.Base(f); base == "doc.go" || base == tp.Name()+".go" {
			best = f
			break
		}
	}
	pf, err := s.parse(best)
	if err != nil {
		return nil
	}
	start := pf.fset.Position(pf.file.Name.Pos()).Offset
	end := pf.fset.Position(pf.file.Name.End()).Offset
	return []lspLocation{{URI: pathToURI(best), Range: lspRange{Start: pf.mapper.position(start), End: pf.mapper.position(end)}}}
}

func (s *server) typeDefinition(file string, pos lspPosition) ([]lspLocation, error) {
	t, err := s.targetAt(file, pos)
	if t == nil || err != nil {
		return nil, err
	}
	typ := t.obj.Type()
	for typ != nil {
		switch u := typ.(type) {
		case *types.Named:
			return s.objectLocation(u.Obj()), nil
		case *types.Alias:
			return s.objectLocation(u.Obj()), nil
		case *types.TypeParam:
			return s.objectLocation(u.Obj()), nil
		case *types.Pointer:
			typ = u.Elem()
		case *types.Slice:
			typ = u.Elem()
		case *types.Array:
			typ = u.Elem()
		case *types.Chan:
			typ = u.Elem()
		case *types.Map:
			typ = u.Elem()
		case *types.Signature:
			if u.Results().Len() == 0 {
				return nil, nil
			}
			typ = u.Results().At(0).Type()
		default:
			return nil, nil
		}
	}
	return nil, nil
}

type hoverResult struct {
	Contents struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	} `json:"contents"`
	Range *lspRange `json:"range,omitempty"`
}

func (s *server) hover(file string, pos lspPosition) (*hoverResult, error) {
	t, err := s.targetAt(file, pos)
	if t == nil || err != nil {
		return nil, err
	}
	qf := func(other *types.Package) string {
		if other == t.pkg.types {
			return ""
		}
		return other.Name()
	}
	sig := types.ObjectString(t.obj, qf)
	doc := ""
	if declFile, _, off, ok := s.objectPos(t.obj); ok {
		if pf, err := s.parse(declFile); err == nil {
			typeText, d := declInfo(pf, off)
			doc = d
			if _, isType := t.obj.(*types.TypeName); isType && typeText != "" {
				sig = typeText
			}
		}
	}
	switch o := t.obj.(type) {
	case *types.Var:
		if o.IsField() {
			sig = "field " + o.Name() + " " + types.TypeString(o.Type(), qf)
		}
	case *types.Const:
		sig += " = " + o.Val().String()
	}

	var b strings.Builder
	b.WriteString("```go\n" + sig + "\n```\n")
	if doc != "" {
		var p comment.Parser
		pr := comment.Printer{}
		b.WriteString("\n---\n\n")
		b.Write(pr.Markdown(p.Parse(doc)))
	}
	if link := s.docLink(t.pkg.view, t.obj); link != "" {
		b.WriteString("\n---\n\n" + link + "\n")
	}
	h := &hoverResult{}
	h.Contents.Kind = "markdown"
	h.Contents.Value = b.String()
	if t.ident != nil {
		if m := t.pkg.mappers[file]; m != nil {
			h.Range = &lspRange{
				Start: m.position(posToken(s.fset, t.ident.Pos()).Offset),
				End:   m.position(posToken(s.fset, t.ident.End()).Offset),
			}
		}
	}
	return h, nil
}

// declInfo looks up the declaration at the offset off of pf.
// It returns the source of the declaration if it declares a type, and the doc comment of the declaration.
func declInfo(pf *parsedFile, off int) (typeText, doc string) {
	tf := pf.fset.File(pf.file.FileStart)
	if off < 0 || off > tf.Size() {
		return "", ""
	}
	path, _ := astutil.PathEnclosingInterval(pf.file, tf.Pos(off), tf.Pos(off))
	for i, n := range path {
		switch n := n.(type) {
		case *ast.FuncDecl:
			return "", n.Doc.Text()
		case *ast.Field:
			if n.Doc != nil {
				return "", n.Doc.Text()
			}
			return "", n.Comment.Text()
		case *ast.TypeSpec:
			var buf bytes.Buffer
			cfg := printer.Config{Mode: printer.UseSpaces | printer.TabIndent, Tabwidth: 4}
			if err := cfg.Fprint(&buf, pf.fset, &printer.CommentedNode{Node: n, Comments: pf.file.Comments}); err == nil {
				typeText = "type " + buf.String()
			}
			doc = n.Doc.Text()
			if doc == "" && i+1 < len(path) {
				if gd, ok := path[i+1].(*ast.GenDecl); ok {
					doc = gd.Doc.Text()
				}
			}
			return typeText, doc
		case *ast.ValueSpec:
			doc = n.Doc.Text()
			if doc == "" {
				doc = n.Comment.Text()
			}
			if doc == "" && i+1 < len(path) {
				if gd, ok := path[i+1].(*ast.GenDecl); ok {
					doc = gd.Doc.Text()
				}
			}
			return "", doc
		case ast.Stmt:
			return "", ""
		}
	}
	return "", ""
}

// docLink returns a link to pkg.go.dev for an exported object declared outside the main module.
func (s *server) docLink(v *view, obj types.Object) string {
	if obj.Pkg() == nil || !obj.Exported() {
		return ""
	}
	anchor := ""
	switch o := obj.(type) {
	case *types.Func:
		anchor = o.Name()
		if recv := o.Signature().Recv(); recv != nil {
			rt := recv.Type()
			if p, ok := rt.(*types.Pointer); ok {
				rt = p.Elem()
			}
			named, ok := rt.(*types.Named)
			if !ok {
				return ""
			}
			anchor = named.Obj().Name() + "." + o.Name()
		}
	case *types.TypeName, *types.Const:
		anchor = obj.Name()
	case *types.Var:
		if o.IsField() || obj.Parent() != obj.Pkg().Scope() {
			return ""
		}
		anchor = obj.Name()
	default:
		return ""
	}
	if _, isFunc := obj.(*types.Func); !isFunc && obj.Parent() != obj.Pkg().Scope() {
		return ""
	}
	m := v.metaOf(obj.Pkg().Path())
	if m == nil || (m.Module != nil && m.Module.Main) {
		return ""
	}
	url := "https://pkg.go.dev/" + m.PkgPath
	if m.Module != nil && m.Module.Version != "" {
		url = "https://pkg.go.dev/" + m.Module.Path + "@" + m.Module.Version + strings.TrimPrefix(m.PkgPath, m.Module.Path)
	}
	return fmt.Sprintf("[`%s.%s` on pkg.go.dev](%s#%s)", obj.Pkg().Name(), anchor, url, anchor)
}

type documentHighlight struct {
	Range lspRange `json:"range"`
	Kind  int      `json:"kind"`
}

func (s *server) documentHighlight(file string, pos lspPosition) ([]documentHighlight, error) {
	t, err := s.targetAt(file, pos)
	if t == nil || err != nil {
		return nil, err
	}
	m := t.pkg.mappers[file]
	var out []documentHighlight
	ast.Inspect(t.file, func(n ast.Node) bool {
		id, ok := n.(*ast.Ident)
		if !ok || t.pkg.info.ObjectOf(id) != t.obj {
			return true
		}
		out = append(out, documentHighlight{
			Range: lspRange{
				Start: m.position(posToken(s.fset, id.Pos()).Offset),
				End:   m.position(posToken(s.fset, id.End()).Offset),
			},
			Kind: 1,
		})
		return true
	})
	return out, nil
}

type documentSymbol struct {
	Name           string           `json:"name"`
	Detail         string           `json:"detail,omitempty"`
	Kind           int              `json:"kind"`
	Range          lspRange         `json:"range"`
	SelectionRange lspRange         `json:"selectionRange"`
	Children       []documentSymbol `json:"children,omitempty"`
}

const (
	kindMethod    = 6
	kindField     = 8
	kindClass     = 5
	kindInterface = 11
	kindFunction  = 12
	kindVariable  = 13
	kindConstant  = 14
	kindStruct    = 23
)

// documentSymbol returns the outline of file. It only parses the file and does not wait for type checking.
func (s *server) documentSymbol(file string) ([]documentSymbol, error) {
	pf, err := s.parse(file)
	if err != nil {
		return nil, err
	}
	rng := func(start, end token.Pos) lspRange {
		return lspRange{
			Start: pf.mapper.position(pf.fset.Position(start).Offset),
			End:   pf.mapper.position(pf.fset.Position(end).Offset),
		}
	}
	text := func(n ast.Node) string {
		var buf bytes.Buffer
		if err := printer.Fprint(&buf, pf.fset, n); err != nil {
			return ""
		}
		return strings.Join(strings.Fields(buf.String()), " ")
	}
	sym := func(name *ast.Ident, kind int, detail string, start, end token.Pos) documentSymbol {
		n := name.Name
		if n == "" {
			n = "_"
		}
		return documentSymbol{Name: n, Detail: detail, Kind: kind, Range: rng(start, end), SelectionRange: rng(name.Pos(), name.End())}
	}
	out := []documentSymbol{}
	for _, d := range pf.file.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			sg := sym(d.Name, kindFunction, text(d.Type), d.Pos(), d.End())
			if d.Recv != nil && len(d.Recv.List) > 0 {
				sg.Kind = kindMethod
				sg.Name = "(" + text(d.Recv.List[0].Type) + ")." + d.Name.Name
			}
			out = append(out, sg)
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				start, end := spec.Pos(), spec.End()
				if !d.Lparen.IsValid() {
					start, end = d.Pos(), d.End()
				}
				switch spec := spec.(type) {
				case *ast.TypeSpec:
					sg := sym(spec.Name, kindClass, "", start, end)
					switch t := spec.Type.(type) {
					case *ast.StructType:
						sg.Kind = kindStruct
						sg.Detail = "struct{...}"
						for _, f := range t.Fields.List {
							for _, n := range f.Names {
								sg.Children = append(sg.Children, sym(n, kindField, text(f.Type), f.Pos(), f.End()))
							}
						}
					case *ast.InterfaceType:
						sg.Kind = kindInterface
						sg.Detail = "interface{...}"
						for _, f := range t.Methods.List {
							for _, n := range f.Names {
								sg.Children = append(sg.Children, sym(n, kindMethod, text(f.Type), f.Pos(), f.End()))
							}
						}
					default:
						sg.Detail = text(spec.Type)
					}
					out = append(out, sg)
				case *ast.ValueSpec:
					kind := kindVariable
					if d.Tok == token.CONST {
						kind = kindConstant
					}
					detail := ""
					if spec.Type != nil {
						detail = text(spec.Type)
					}
					for _, n := range spec.Names {
						out = append(out, sym(n, kind, detail, start, end))
					}
				}
			}
		}
	}
	return out, nil
}
