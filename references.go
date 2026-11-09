package main

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/types"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/types/objectpath"
)

// maxReferencePackages is the maximum number of packages per module that a single reference search type-checks.
// It keeps a name that appears in almost every file, such as a type of the standard library, from type-checking a whole module.
const maxReferencePackages = 400

// maxKeptReferencePackages is the maximum number of packages whose type-checked results a reference search keeps.
// A search that spans only a few packages does not type-check them again when it is repeated.
const maxKeptReferencePackages = 16

// objectKey identifies an object across separate type-checking runs.
// The same package has different types.Object values when it is loaded as a dependency and when it is type-checked with function bodies,
// so objects cannot be compared by pointer.
type objectKey struct {
	pkg  string
	path objectpath.Path
}

func keyOf(obj types.Object) (objectKey, bool) {
	switch o := obj.(type) {
	case *types.Func:
		obj = o.Origin()
	case *types.Var:
		obj = o.Origin()
	}
	if obj.Pkg() == nil {
		return objectKey{}, false
	}
	path, err := objectpath.For(obj)
	if err != nil {
		return objectKey{}, false
	}
	return objectKey{pkg: obj.Pkg().Path(), path: path}, true
}

// refVisit is called for each reference. id is an identifier in the file named file of p, whose syntax tree is f.
type refVisit func(p *fullPkg, file string, f *ast.File, id *ast.Ident)

// eachReference calls visit for each identifier that refers to the object of t. It never calls visit concurrently.
// When all is true, it searches every module in the workspace, even for an object declared in the standard library or a third-party module.
// When limitedMethod is not empty and the search is narrowed to the module of the open file, it notifies the client under that name.
func (s *server) eachReference(t *target, file string, pos lspPosition, includeDecl, all bool, limitedMethod string, visit refVisit) {
	start := time.Now()
	var mu sync.Mutex
	seen := map[string]bool{}
	emit := func(p *fullPkg, match func(types.Object) bool) {
		for name, f := range p.files {
			if s.skipPath(name) {
				continue
			}
			ast.Inspect(f, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				obj := p.info.Uses[id]
				if obj == nil && includeDecl {
					obj = p.info.Defs[id]
				}
				if obj == nil || !match(obj) {
					return true
				}
				// A package rebuilt for tests, or the same package loaded from another module, has the same files. Do not report a position twice.
				key := fmt.Sprintf("%s:%d", name, posToken(s.fset, id.Pos()).Offset)
				mu.Lock()
				if !seen[key] {
					seen[key] = true
					visit(p, name, f, id)
				}
				mu.Unlock()
				return true
			})
		}
	}

	key, global := keyOf(t.obj)
	if !global {
		// An object that is not visible outside its package, such as a local variable, is searched only in the open package.
		emit(t.pkg, func(obj types.Object) bool { return obj == t.obj })
		return
	}
	origin := t.pkg.view
	decl := declMeta(t, t.obj.Pkg())
	declDir := ""
	if decl != nil && len(decl.GoFiles) > 0 {
		declDir = filepath.Dir(decl.GoFiles[0])
	}
	match := func(obj types.Object) bool {
		if obj.Name() != t.obj.Name() || obj.Pkg() == nil || obj.Pkg().Path() != key.pkg {
			return false
		}
		k, ok := keyOf(obj)
		return ok && k == key
	}
	roots, limited := s.searchRoots(origin, decl, all)
	if limited && limitedMethod != "" {
		s.notifyLimited(limitedMethod, file, pos, t.obj.Name())
	}
	var wg sync.WaitGroup
	truncated := false
	for _, root := range roots {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rootStart := time.Now()
			v := s.viewByRoot(root)
			files := v.candidateFiles(t.obj, declDir)
			ids := v.packagesOf(files)
			if v == origin && !contains(ids, t.pkg.meta.ID) {
				ids = append(ids, t.pkg.meta.ID)
			}
			if len(ids) > maxReferencePackages {
				ids = ids[:maxReferencePackages]
				mu.Lock()
				truncated = true
				mu.Unlock()
			}
			var pkgWG sync.WaitGroup
			for _, id := range ids {
				pkgWG.Add(1)
				go func() {
					defer pkgWG.Done()
					if p := v.fullTransient(id, len(ids) <= maxKeptReferencePackages); p != nil {
						emit(p, match)
					}
				}()
			}
			pkgWG.Wait()
			hits, misses := v.sharedStats()
			logf("references %s in %s: %d files, %d packages in %v (shared types: %d reused, %d loaded)", t.obj.Name(), root, len(files), len(ids), time.Since(rootStart), hits, misses)
		}()
	}
	wg.Wait()
	if truncated {
		// Show a warning so that the user does not miss that the results are partial.
		_ = s.conn.notify("window/showMessage", map[string]any{
			"type":    2,
			"message": s.localize(msgReferencesTruncated, t.obj.Name(), maxReferencePackages),
		})
	}
	logf("references %s: %d modules (truncated: %v), %d results in %v", t.obj.Name(), len(roots), truncated, len(seen), time.Since(start))
}

func (s *server) references(file string, pos lspPosition, includeDecl, all bool) ([]lspLocation, error) {
	t, err := s.targetAt(file, pos)
	if t == nil || err != nil {
		return nil, err
	}
	out := []lspLocation{}
	s.eachReference(t, file, pos, includeDecl, all, "references", func(p *fullPkg, name string, _ *ast.File, id *ast.Ident) {
		out = append(out, lspLocation{URI: pathToURI(name), Range: p.identRange(name, id)})
	})
	sortLocations(out)
	return out, nil
}

// identRange returns the range of the identifier id in the file named file of p.
func (p *fullPkg) identRange(file string, id *ast.Ident) lspRange {
	m := p.mappers[file]
	fset := p.view.s.fset
	return lspRange{
		Start: m.position(posToken(fset, id.Pos()).Offset),
		End:   m.position(posToken(fset, id.End()).Offset),
	}
}

func sortLocations(out []lspLocation) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].URI != out[j].URI {
			return out[i].URI < out[j].URI
		}
		if out[i].Range.Start.Line != out[j].Range.Start.Line {
			return out[i].Range.Start.Line < out[j].Range.Start.Line
		}
		return out[i].Range.Start.Character < out[j].Range.Start.Character
	})
}

// declMeta returns the metadata of pkg, the package that declares the object of t.
func declMeta(t *target, pkg *types.Package) *packages.Package {
	if pkg == nil {
		return nil
	}
	if pkg == t.pkg.types {
		return t.pkg.meta
	}
	return t.pkg.view.metaOf(pkg.Path())
}

// candidateFiles returns the Go files that may refer to obj, narrowed down by a text search over the sources of the main module.
// A candidate contains the name of obj as a whole word.
// A package-level function, type, variable or constant is referred to with its package name,
// so candidates are narrowed to the files that import the package of obj or are in its directory.
// A method or a field can be referred to from a file that does not import the declaring package, so imports do not narrow them.
// This avoids loading the whole module with go list ./... .
// declDir is the directory of the package that declares obj.
func (v *view) candidateFiles(obj types.Object, declDir string) []string {
	name := []byte(obj.Name())
	importLit := []byte(`"` + obj.Pkg().Path() + `"`)
	needImport := obj.Parent() == obj.Pkg().Scope()
	return v.searchFiles(func(path string, src []byte) bool {
		if !containsWord(src, name) {
			return false
		}
		return !needImport || filepath.Dir(path) == declDir || bytes.Contains(src, importLit)
	})
}

// searchFiles returns the paths of the Go files of the main module that searches do not leave out and for which match returns true.
func (v *view) searchFiles(match func(path string, src []byte) bool) []string {
	paths := make(chan string, 256)
	var mu sync.Mutex
	var out []string
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range paths {
				src, err := v.s.readFile(path)
				if err != nil || !match(path, src) {
					continue
				}
				mu.Lock()
				out = append(out, path)
				mu.Unlock()
			}
		}()
	}
	walkGoFiles(v.root, true, func(path string) {
		if !v.s.skipPath(path) {
			paths <- path
		}
	})
	close(paths)
	wg.Wait()
	sort.Strings(out)
	return out
}

// walkGoFiles calls fn for each Go file under root. It skips hidden directories, node_modules, vendor and testdata.
// When stopAtModules is true, it also skips the nested directories that have a go.mod, which are other modules.
func walkGoFiles(root string, stopAtModules bool, fn func(path string)) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		base := d.Name()
		if d.IsDir() {
			if path == root {
				return nil
			}
			if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") || base == "node_modules" || base == "vendor" || base == "testdata" {
				return filepath.SkipDir
			}
			if stopAtModules {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasSuffix(base, ".go") {
			fn(path)
		}
		return nil
	})
}

func containsWord(src, word []byte) bool {
	for i := 0; ; {
		j := bytes.Index(src[i:], word)
		if j < 0 {
			return false
		}
		if isWordAt(src, i+j, string(word)) {
			return true
		}
		i += j + 1
	}
}

// packagesOf returns the IDs of the packages that contain files.
// It loads the metadata of the files it does not know yet with a single go list.
func (v *view) packagesOf(files []string) []string {
	var unknown []string
	for _, f := range files {
		if v.pickID(f) == "" {
			unknown = append(unknown, f)
		}
	}
	if len(unknown) > 0 {
		v.loadMetaFiles(unknown)
	}
	seen := map[string]bool{}
	var ids []string
	for _, f := range files {
		if id := v.pickID(f); id != "" && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids
}

// loadMetaFiles loads the metadata of the packages in the directories of files with a single go list.
// It passes directories because a "file=" query runs go list once per file, which is slow.
func (v *view) loadMetaFiles(files []string) {
	start := time.Now()
	cfg := &packages.Config{Mode: metaMode, Dir: v.root}
	seen := map[string]bool{}
	var patterns []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			cfg.Tests = true
		}
		if dir := filepath.Dir(f); !seen[dir] {
			seen[dir] = true
			patterns = append(patterns, dir)
		}
	}
	v.loadMu.Lock()
	defer v.loadMu.Unlock()
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		logf("loadMetaFiles: %v", err)
		return
	}
	added := v.addMeta(pkgs)
	v.recordMeta(pkgs)
	logf("loadMetaFiles: %d files in %d directories, %d packages added in %v", len(files), len(patterns), added, time.Since(start))
}

// fullTransient returns the package id type-checked with function bodies.
// When keep is false, it does not keep the result unless the package has an open file,
// so that reference searches spanning many packages do not keep growing the memory.
func (v *view) fullTransient(id string, keep bool) *fullPkg {
	v.mu.Lock()
	e := v.fulls[id]
	v.mu.Unlock()
	if e != nil || keep {
		return v.full(id)
	}
	m := v.metaOf(id)
	if m == nil {
		return nil
	}
	return v.buildFull(m)
}

// sharedStats returns how many packages this view reused from other views and how many it loaded itself.
func (v *view) sharedStats() (hits, misses int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.sharedHits, v.sharedMisses
}
