package main

import (
	"bytes"
	"go/types"
	"sort"
	"sync"
	"time"
)

// maxEmbedderNames is the maximum number of type names passed to the text search
// that looks for types implementing an interface only through embedding.
const maxEmbedderNames = 64

// implDir is the direction of an implementation search.
type implDir int

const (
	// dirImplementers looks for the non-interface types that implement an interface.
	dirImplementers implDir = iota
	// dirSubtypes looks for the types that implement an interface, including interfaces.
	dirSubtypes
	// dirInterfaces looks for the interfaces that a type satisfies.
	dirInterfaces
)

// implTarget is the starting point of an implementation search.
type implTarget struct {
	named *types.Named
	// method is the method name when the search starts from a method. It is empty when the search starts from a type.
	method string
}

func (t *implTarget) isInterface() bool {
	_, ok := t.named.Underlying().(*types.Interface)
	return ok
}

func implTargetOf(obj types.Object) *implTarget {
	var typ types.Type
	method := ""
	switch o := obj.(type) {
	case *types.TypeName:
		typ = o.Type()
	case *types.Func:
		recv := o.Signature().Recv()
		if recv == nil {
			return nil
		}
		typ = recv.Type()
		if p, ok := typ.(*types.Pointer); ok {
			typ = p.Elem()
		}
		method = o.Name()
	default:
		return nil
	}
	named, ok := types.Unalias(typ).(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return nil
	}
	// An instantiated type such as List[int] is handled as its declaration List[T].
	return &implTarget{named: named.Origin(), method: method}
}

// methodNames returns the names of the methods callable on typ and on its pointer type.
func methodNames(typ types.Type) []string {
	if _, ok := typ.Underlying().(*types.Interface); !ok {
		typ = types.NewPointer(typ)
	}
	set := types.NewMethodSet(typ)
	names := make([]string, 0, set.Len())
	for i := range set.Len() {
		names = append(names, set.At(i).Obj().Name())
	}
	return names
}

// shapeImplements reports whether v, or its pointer type, has every method of iface with a signature of the same shape.
//
// It does not use types.Implements, which cannot match the following three cases.
//   - Types and interfaces with type parameters. A type parameter is treated as matching any type.
//   - Types of a package rebuilt for tests. The same declaration gets a different types.Object.
//   - Types loaded in another module (view). Each set of dependency versions gets a different types.Object.
//
// So named types are compared by package path and name instead of by types.Object.
func shapeImplements(v *types.Named, iface *types.Interface) bool {
	if iface.NumMethods() == 0 || !iface.IsMethodSet() {
		return false
	}
	var recv types.Type = v
	if _, ok := v.Underlying().(*types.Interface); !ok {
		recv = types.NewPointer(v)
	}
	for i := range iface.NumMethods() {
		m := iface.Method(i)
		obj, _, _ := types.LookupFieldOrMethod(recv, true, m.Pkg(), m.Name())
		f, ok := obj.(*types.Func)
		if !ok {
			// An unexported method cannot be looked up when the types.Object of its package differs.
			// Look it up again by package path.
			if f = lookupUnexported(recv, m); f == nil {
				return false
			}
		}
		if !unify(f.Signature(), m.Signature(), 0) {
			return false
		}
	}
	return true
}

func lookupUnexported(recv types.Type, m *types.Func) *types.Func {
	if m.Exported() || m.Pkg() == nil {
		return nil
	}
	set := types.NewMethodSet(recv)
	for i := range set.Len() {
		f, ok := set.At(i).Obj().(*types.Func)
		if ok && f.Name() == m.Name() && f.Pkg() != nil && f.Pkg().Path() == m.Pkg().Path() {
			return f
		}
	}
	return nil
}

// unify reports whether a and b are types of the same shape. A type parameter is treated as matching any type.
func unify(a, b types.Type, depth int) bool {
	if depth > 32 {
		return true
	}
	a, b = types.Unalias(a), types.Unalias(b)
	if _, ok := a.(*types.TypeParam); ok {
		return true
	}
	if _, ok := b.(*types.TypeParam); ok {
		return true
	}
	switch a := a.(type) {
	case *types.Basic:
		b, ok := b.(*types.Basic)
		return ok && a.Kind() == b.Kind()
	case *types.Named:
		b, ok := b.(*types.Named)
		if !ok {
			return false
		}
		ao, bo := a.Origin().Obj(), b.Origin().Obj()
		if ao.Name() != bo.Name() || pkgPath(ao) != pkgPath(bo) {
			return false
		}
		at, bt := a.TypeArgs(), b.TypeArgs()
		if at.Len() == bt.Len() {
			for i := range at.Len() {
				if !unify(at.At(i), bt.At(i), depth+1) {
					return false
				}
			}
		}
		return true
	case *types.Pointer:
		b, ok := b.(*types.Pointer)
		return ok && unify(a.Elem(), b.Elem(), depth+1)
	case *types.Slice:
		b, ok := b.(*types.Slice)
		return ok && unify(a.Elem(), b.Elem(), depth+1)
	case *types.Array:
		b, ok := b.(*types.Array)
		return ok && a.Len() == b.Len() && unify(a.Elem(), b.Elem(), depth+1)
	case *types.Chan:
		b, ok := b.(*types.Chan)
		return ok && a.Dir() == b.Dir() && unify(a.Elem(), b.Elem(), depth+1)
	case *types.Map:
		b, ok := b.(*types.Map)
		return ok && unify(a.Key(), b.Key(), depth+1) && unify(a.Elem(), b.Elem(), depth+1)
	case *types.Signature:
		b, ok := b.(*types.Signature)
		return ok && a.Variadic() == b.Variadic() && unify(a.Params(), b.Params(), depth+1) && unify(a.Results(), b.Results(), depth+1)
	case *types.Tuple:
		b, ok := b.(*types.Tuple)
		if !ok || a.Len() != b.Len() {
			return false
		}
		for i := range a.Len() {
			if !unify(a.At(i).Type(), b.At(i).Type(), depth+1) {
				return false
			}
		}
		return true
	case *types.Struct:
		b, ok := b.(*types.Struct)
		if !ok || a.NumFields() != b.NumFields() {
			return false
		}
		for i := range a.NumFields() {
			af, bf := a.Field(i), b.Field(i)
			if af.Name() != bf.Name() || af.Embedded() != bf.Embedded() || !unify(af.Type(), bf.Type(), depth+1) {
				return false
			}
		}
		return true
	case *types.Interface:
		b, ok := b.(*types.Interface)
		if !ok || a.NumMethods() != b.NumMethods() {
			return false
		}
		for i := range a.NumMethods() {
			am, bm := a.Method(i), b.Method(i)
			if am.Name() != bm.Name() || !unify(am.Signature(), bm.Signature(), depth+1) {
				return false
			}
		}
		return true
	}
	return types.Identical(a, b)
}

func pkgPath(obj types.Object) string {
	if obj.Pkg() == nil {
		return ""
	}
	return obj.Pkg().Path()
}

func sameNamed(a, b *types.Named) bool {
	return a.Obj().Name() == b.Obj().Name() && pkgPath(a.Obj()) == pkgPath(b.Obj())
}

// findImplementations returns the types in the workspace that match target and dir.
// The candidates are the packages narrowed down by a text search for method names, plus the packages already loaded,
// which include the standard library and third-party modules.
// When limitedMethod is not empty and the search is narrowed to the module of the open file, it notifies the client under that name.
func (s *server) findImplementations(t *target, file string, pos lspPosition, target *implTarget, dir implDir, all bool, limitedMethod string) []*types.TypeName {
	start := time.Now()
	tn := target.named.Obj()
	names := methodNames(target.named)
	if len(names) == 0 {
		return nil
	}
	if target.method != "" {
		names = []string{target.method}
	}
	// A type that implements an interface has all its methods, so the single longest name narrows the candidates.
	// An interface requires only some of the methods of a type, so a file with any one of the names is a candidate.
	need := []byte("interface")
	if dir != dirInterfaces {
		need = []byte("func (")
		if target.method == "" {
			sort.Slice(names, func(i, j int) bool { return len(names[i]) > len(names[j]) })
			names = names[:1]
		}
	}
	anyWord := func(src []byte, words []string) bool {
		for _, w := range words {
			if containsWord(src, []byte(w)) {
				return true
			}
		}
		return false
	}

	origin := t.pkg.view
	roots, limited := s.searchRoots(origin, declMeta(t, tn.Pkg()), all)
	if limited && limitedMethod != "" {
		s.notifyLimited(limitedMethod, file, pos, tn.Name())
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	seen := map[string]bool{}
	var out []*types.TypeName
	truncated := false
	for _, root := range roots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rootStart := time.Now()
			v := s.viewByRoot(root)
			files := v.searchFiles(func(_ string, src []byte) bool { return bytes.Contains(src, need) && anyWord(src, names) })
			if v != origin && len(files) == 0 {
				return
			}
			loaded := map[string]bool{}
			load := func(files []string) {
				var ids []string
				for _, id := range v.packagesOf(files) {
					if !loaded[id] {
						loaded[id] = true
						ids = append(ids, id)
					}
				}
				if len(loaded) > maxReferencePackages {
					mu.Lock()
					truncated = true
					mu.Unlock()
					return
				}
				waiting := make([]*depPkg, 0, len(ids))
				for _, id := range ids {
					waiting = append(waiting, v.dep(id))
				}
				for _, d := range waiting {
					<-d.done
				}
			}
			load(files)
			found := v.scanImplementations(target.named, dir)

			// The file of a type that gets its methods only through struct embedding does not contain the method names.
			// Widen the candidates to the files that may embed the starting interface or one of the types found so far.
			if dir != dirInterfaces {
				searched := map[string]bool{}
				for range 2 {
					words := []string{}
					for _, name := range append([]string{tn.Name()}, typeNames(found)...) {
						if !searched[name] && len(words) < maxEmbedderNames {
							searched[name] = true
							words = append(words, name)
						}
					}
					if len(words) == 0 {
						break
					}
					structWord := []byte("struct")
					more := v.searchFiles(func(_ string, src []byte) bool { return bytes.Contains(src, structWord) && anyWord(src, words) })
					before := len(found)
					load(more)
					found = v.scanImplementations(target.named, dir)
					if len(found) == before {
						break
					}
				}
			}

			mu.Lock()
			defer mu.Unlock()
			for _, cand := range found {
				key := pkgPath(cand) + "." + cand.Name()
				if !seen[key] {
					seen[key] = true
					out = append(out, cand)
				}
			}
			logf("implementation %s in %s: %d files, %d candidate packages, %d found in %v", tn.Name(), root, len(files), len(loaded), len(found), time.Since(rootStart))
		}()
	}
	wg.Wait()
	if truncated {
		_ = s.conn.notify("window/showMessage", map[string]any{
			"type":    2,
			"message": s.localize(msgImplementationsTruncated, tn.Name(), maxReferencePackages),
		})
	}
	// The built-in error belongs to no package, so check it separately.
	if errType := types.Universe.Lookup("error").(*types.TypeName); dir == dirInterfaces && shapeImplements(target.named, errType.Type().Underlying().(*types.Interface)) {
		out = append(out, errType)
	}
	sort.Slice(out, func(i, j int) bool {
		if pkgPath(out[i]) != pkgPath(out[j]) {
			return pkgPath(out[i]) < pkgPath(out[j])
		}
		return out[i].Name() < out[j].Name()
	})
	logf("implementation %s: %d modules (truncated: %v), %d results in %v", tn.Name(), len(roots), truncated, len(out), time.Since(start))
	return out
}

func typeNames(list []*types.TypeName) []string {
	names := make([]string, 0, len(list))
	for _, tn := range list {
		names = append(names, tn.Name())
	}
	return names
}

// scanImplementations looks for the types that match named and dir among the packages loaded by v and the packages shared between views.
// The shared packages include other versions used by other modules.
// Types are compared by name and signature shape, so the version difference does not prevent matching.
func (v *view) scanImplementations(named *types.Named, dir implDir) []*types.TypeName {
	targetIface, _ := named.Underlying().(*types.Interface)
	seen := map[string]bool{}
	var found []*types.TypeName
	for _, pkg := range v.s.loadedDeps(v) {
		scope := pkg.Scope()
		for _, name := range scope.Names() {
			cand, ok := scope.Lookup(name).(*types.TypeName)
			if !ok || cand.IsAlias() {
				continue
			}
			candNamed, ok := cand.Type().(*types.Named)
			if !ok || sameNamed(candNamed, named) {
				continue
			}
			candIface, candIsInterface := candNamed.Underlying().(*types.Interface)
			var matched bool
			switch dir {
			case dirImplementers:
				matched = targetIface != nil && !candIsInterface && shapeImplements(candNamed, targetIface)
			case dirSubtypes:
				matched = targetIface != nil && shapeImplements(candNamed, targetIface)
			case dirInterfaces:
				matched = candIsInterface && shapeImplements(named, candIface)
			}
			// A package rebuilt for tests has the same types as the regular package. Do not return a type twice.
			if key := pkgPath(cand) + "." + cand.Name(); matched && !seen[key] {
				seen[key] = true
				found = append(found, cand)
			}
		}
	}
	return found
}

// implementation returns the types that implement an interface, or the interfaces that a concrete type satisfies.
// On a method, it returns the declarations of the corresponding methods instead of the types.
// When all is true, it searches every module in the workspace, even for a type declared in the standard library or a third-party module.
func (s *server) implementation(file string, pos lspPosition, all bool) ([]lspLocation, error) {
	t, err := s.targetAt(file, pos)
	if t == nil || err != nil {
		return nil, err
	}
	target := implTargetOf(t.obj)
	if target == nil {
		return nil, nil
	}
	dir := dirInterfaces
	if target.isInterface() {
		dir = dirImplementers
	}
	// A type that embeds the starting interface gets the starting method itself. Do not return the starting point.
	seen := map[lspLocation]bool{}
	for _, l := range s.objectLocation(t.obj) {
		seen[l] = true
	}
	out := []lspLocation{}
	for _, cand := range s.findImplementations(t, file, pos, target, dir, all, "implementation") {
		var obj types.Object = cand
		if target.method != "" && cand.Pkg() != nil {
			// When the search starts from a method, return the declaration of the corresponding method instead of the type.
			recv := cand.Type()
			if _, ok := recv.Underlying().(*types.Interface); !ok {
				recv = types.NewPointer(recv)
			}
			m, _, _ := types.LookupFieldOrMethod(recv, true, cand.Pkg(), target.method)
			if _, ok := m.(*types.Func); !ok {
				continue
			}
			obj = m
		}
		for _, l := range s.objectLocation(obj) {
			if !seen[l] {
				seen[l] = true
				out = append(out, l)
			}
		}
	}
	out = s.filterLocations(out)
	sortLocations(out)
	return out, nil
}
