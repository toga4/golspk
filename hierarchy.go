package main

import (
	"go/ast"
	"go/types"
	"sort"

	"golang.org/x/tools/go/ast/astutil"
)

// hierarchyItem is an item of a call hierarchy or a type hierarchy.
// CallHierarchyItem and TypeHierarchyItem of LSP have the same shape.
type hierarchyItem struct {
	Name           string   `json:"name"`
	Kind           int      `json:"kind"`
	Detail         string   `json:"detail,omitempty"`
	URI            string   `json:"uri"`
	Range          lspRange `json:"range"`
	SelectionRange lspRange `json:"selectionRange"`
}

type incomingCall struct {
	From       hierarchyItem `json:"from"`
	FromRanges []lspRange    `json:"fromRanges"`
}

type outgoingCall struct {
	To         hierarchyItem `json:"to"`
	FromRanges []lspRange    `json:"fromRanges"`
}

// displayName returns the name of obj to show in a hierarchy. A method is prefixed with the type name of its receiver.
func displayName(obj types.Object) string {
	if f, ok := obj.(*types.Func); ok {
		if recv := f.Signature().Recv(); recv != nil {
			rt := recv.Type()
			if p, ok := rt.(*types.Pointer); ok {
				rt = p.Elem()
			}
			if named, ok := types.Unalias(rt).(*types.Named); ok {
				return named.Obj().Name() + "." + f.Name()
			}
		}
	}
	return obj.Name()
}

func kindOf(obj types.Object) int {
	switch o := obj.(type) {
	case *types.Func:
		if o.Signature().Recv() != nil {
			return kindMethod
		}
		return kindFunction
	case *types.TypeName:
		switch o.Type().Underlying().(type) {
		case *types.Interface:
			return kindInterface
		case *types.Struct:
			return kindStruct
		}
		return kindClass
	}
	return kindVariable
}

// itemFor returns the hierarchy item that points at the declaration of obj, or nil if the position of the declaration is unknown.
func (s *server) itemFor(obj types.Object) *hierarchyItem {
	locs := s.objectLocation(obj)
	if len(locs) == 0 {
		return nil
	}
	return &hierarchyItem{
		Name:           displayName(obj),
		Kind:           kindOf(obj),
		Detail:         pkgPath(obj),
		URI:            locs[0].URI,
		Range:          locs[0].Range,
		SelectionRange: locs[0].Range,
	}
}

func sortItems[T any](list []T, item func(T) hierarchyItem) {
	sort.Slice(list, func(i, j int) bool {
		a, b := item(list[i]), item(list[j])
		if a.URI != b.URI {
			return a.URI < b.URI
		}
		return a.SelectionRange.Start.Line < b.SelectionRange.Start.Line
	})
}

// prepareCallHierarchy returns the function or the method under the cursor as the root of a call hierarchy.
func (s *server) prepareCallHierarchy(file string, pos lspPosition) ([]hierarchyItem, error) {
	t, err := s.targetAt(file, pos)
	if t == nil || err != nil {
		return nil, err
	}
	if _, ok := t.obj.(*types.Func); !ok {
		return nil, nil
	}
	if item := s.itemFor(t.obj); item != nil {
		return []hierarchyItem{*item}, nil
	}
	return nil, nil
}

// incomingCalls returns the functions that refer to the function of item. It searches the same scope as find references.
func (s *server) incomingCalls(item hierarchyItem) ([]incomingCall, error) {
	file, pos := uriToPath(item.URI), item.SelectionRange.Start
	t, err := s.targetAt(file, pos)
	if t == nil || err != nil {
		return nil, err
	}
	if _, ok := t.obj.(*types.Func); !ok {
		return nil, nil
	}
	calls := map[lspLocation]*incomingCall{}
	s.eachReference(t, file, pos, false, false, "", func(p *fullPkg, name string, f *ast.File, id *ast.Ident) {
		// The caller is the outermost function declaration that contains the reference.
		// A reference inside a function literal counts as a call from the function that contains the literal.
		path, _ := astutil.PathEnclosingInterval(f, id.Pos(), id.Pos())
		var caller *ast.FuncDecl
		for _, n := range path {
			if fd, ok := n.(*ast.FuncDecl); ok {
				caller = fd
			}
		}
		if caller == nil {
			return
		}
		obj := p.info.Defs[caller.Name]
		if obj == nil {
			return
		}
		rng := p.identRange(name, caller.Name)
		key := lspLocation{URI: pathToURI(name), Range: rng}
		c := calls[key]
		if c == nil {
			c = &incomingCall{From: hierarchyItem{Name: displayName(obj), Kind: kindOf(obj), Detail: pkgPath(obj), URI: key.URI, Range: rng, SelectionRange: rng}}
			calls[key] = c
		}
		c.FromRanges = append(c.FromRanges, p.identRange(name, id))
	})
	out := make([]incomingCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, *c)
	}
	sortItems(out, func(c incomingCall) hierarchyItem { return c.From })
	return out, nil
}

// outgoingCalls returns the functions and the methods that the body of the function of item calls.
func (s *server) outgoingCalls(item hierarchyItem) ([]outgoingCall, error) {
	file := uriToPath(item.URI)
	t, err := s.targetAt(file, item.SelectionRange.Start)
	if t == nil || err != nil {
		return nil, err
	}
	var decl *ast.FuncDecl
	for _, d := range t.file.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name == t.ident {
			decl = fd
		}
	}
	if decl == nil || decl.Body == nil {
		return nil, nil
	}
	calls := map[types.Object]*outgoingCall{}
	var order []types.Object
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fun := ast.Unparen(call.Fun)
		// For a call with explicit type arguments such as F[int](x), strip the index to get the function name.
		switch e := fun.(type) {
		case *ast.IndexExpr:
			fun = e.X
		case *ast.IndexListExpr:
			fun = e.X
		}
		var id *ast.Ident
		switch e := fun.(type) {
		case *ast.Ident:
			id = e
		case *ast.SelectorExpr:
			id = e.Sel
		}
		if id == nil {
			return true
		}
		callee, ok := t.pkg.info.Uses[id].(*types.Func)
		if !ok {
			return true
		}
		callee = callee.Origin()
		c := calls[callee]
		if c == nil {
			to := s.itemFor(callee)
			if to == nil {
				return true
			}
			c = &outgoingCall{To: *to}
			calls[callee] = c
			order = append(order, callee)
		}
		c.FromRanges = append(c.FromRanges, t.pkg.identRange(file, id))
		return true
	})
	out := make([]outgoingCall, 0, len(order))
	for _, obj := range order {
		out = append(out, *calls[obj])
	}
	return out, nil
}

// prepareTypeHierarchy returns the type under the cursor as the root of a type hierarchy.
func (s *server) prepareTypeHierarchy(file string, pos lspPosition) ([]hierarchyItem, error) {
	t, err := s.targetAt(file, pos)
	if t == nil || err != nil {
		return nil, err
	}
	target := implTargetOf(t.obj)
	if target == nil || target.method != "" {
		return nil, nil
	}
	if item := s.itemFor(target.named.Obj()); item != nil {
		return []hierarchyItem{*item}, nil
	}
	return nil, nil
}

// typeHierarchy returns the supertypes (the interfaces the type satisfies) or the subtypes (the types that implement it) of the type of item.
func (s *server) typeHierarchy(item hierarchyItem, dir implDir) ([]hierarchyItem, error) {
	file, pos := uriToPath(item.URI), item.SelectionRange.Start
	t, err := s.targetAt(file, pos)
	if t == nil || err != nil {
		return nil, err
	}
	target := implTargetOf(t.obj)
	if target == nil || target.method != "" {
		return nil, nil
	}
	if dir == dirSubtypes && !target.isInterface() {
		// A type that is not an interface has no subtypes.
		return []hierarchyItem{}, nil
	}
	out := []hierarchyItem{}
	for _, cand := range s.findImplementations(t, file, pos, target, dir, false, "") {
		if it := s.itemFor(cand); it != nil && !s.skipPath(uriToPath(it.URI)) {
			out = append(out, *it)
		}
	}
	sortItems(out, func(i hierarchyItem) hierarchyItem { return i })
	return out, nil
}
