package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"go/version"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/tools/go/gcexportdata"
	"golang.org/x/tools/go/packages"
)

// cacheFormat is the version of the format of the type information saved on disk. Bump it when the saved content changes.
const cacheFormat = "1"

const metaMode = packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles | packages.NeedImports | packages.NeedDeps | packages.NeedModule

// cpuSem limits the number of concurrent parsing and type-checking jobs to the number of CPUs.
var cpuSem = make(chan struct{}, runtime.GOMAXPROCS(0))

type goEnv struct {
	GOVERSION   string
	GOOS        string
	GOARCH      string
	GOMODCACHE  string
	GOROOT      string
	CGO_ENABLED string
}

// view holds the packages loaded under one go.mod, the main module.
// Dependency versions differ between main modules, so each view resolves packages on its own.
// Packages that turn out to be identical are shared between views through server.shared.
type view struct {
	s    *server
	root string

	envOnce sync.Once
	// loadMu serializes go list runs, so that two requests do not load the metadata of the same file at the same time.
	loadMu sync.Mutex
	env    goEnv

	mu        sync.Mutex
	meta      map[string]*packages.Package // package ID to metadata
	fileIDs   map[string][]string          // file path to the IDs of the packages that contain it
	deps      map[string]*depPkg           // packages type-checked as dependencies, without function bodies
	fulls     map[string]*fullEntry        // packages of open files, type-checked with function bodies
	immutable map[string]bool
	keys      map[string]string

	// sharedHits and sharedMisses count the packages this view reused from other views and the packages it loaded itself.
	sharedHits, sharedMisses int
}

// depPkg is the type information of a package loaded as a dependency. Read types only after done is closed.
type depPkg struct {
	done  chan struct{}
	types *types.Package
	// mutable reports whether the content of the package can change, as with a package of the workspace.
	// Such a package is dropped from the shared pool once no view uses it.
	mutable bool
}

type fullEntry struct {
	mu  sync.Mutex
	pkg *fullPkg
}

// fullPkg is a package type-checked with function bodies.
type fullPkg struct {
	view    *view
	meta    *packages.Package
	stamp   string
	files   map[string]*ast.File
	mappers map[string]*mapper
	types   *types.Package
	info    *types.Info
	// missingImport reports whether the package imports something that the metadata lacks.
	// It is the signal to load the metadata again.
	missingImport bool
}

func newView(s *server, root string) *view {
	return &view{
		s:         s,
		root:      root,
		meta:      map[string]*packages.Package{},
		fileIDs:   map[string][]string{},
		deps:      map[string]*depPkg{},
		fulls:     map[string]*fullEntry{},
		immutable: map[string]bool{},
		keys:      map[string]string{},
	}
}

func (v *view) goEnv() goEnv {
	v.envOnce.Do(func() {
		cmd := exec.Command("go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "GOMODCACHE", "GOROOT", "CGO_ENABLED")
		cmd.Dir = v.root
		out, err := cmd.Output()
		if err != nil {
			logf("go env: %v", err)
			return
		}
		if err := json.Unmarshal(out, &v.env); err != nil {
			logf("go env: %v", err)
		}
	})
	return v.env
}

// loadMeta loads the metadata of the package that contains file and of its dependencies with go list.
// It neither type-checks nor compiles, so it finishes within a second even with more than 1000 dependencies.
func (v *view) loadMeta(file string) error {
	start := time.Now()
	cfg := &packages.Config{
		Mode:  metaMode,
		Dir:   filepath.Dir(file),
		Tests: strings.HasSuffix(file, "_test.go"),
	}
	pkgs, err := packages.Load(cfg, "file="+file)
	if err != nil {
		return err
	}
	added := v.addMeta(pkgs)
	v.recordMeta(pkgs)
	logf("loadMeta %s: %d packages added in %v", file, added, time.Since(start))
	return nil
}

// recordMeta records the module files of the view and the directories of the packages in pkgs whose files can change,
// so that checkDisk notices files added to them and changes of the dependencies.
func (v *view) recordMeta(pkgs []*packages.Package) {
	if !v.s.pollDisk.Load() {
		return
	}
	for _, name := range []string{"go.mod", "go.sum", "go.work"} {
		v.s.recordNewFile(filepath.Join(v.root, name))
	}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if len(p.GoFiles) > 0 && !v.selfImmutable(p) {
			v.s.recordDir(filepath.Dir(p.GoFiles[0]))
		}
	})
}

// addMeta adds the metadata of pkgs and their dependencies that the view does not have yet, and returns the number added.
func (v *view) addMeta(pkgs []*packages.Package) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	added := 0
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if _, ok := v.meta[p.ID]; ok {
			return
		}
		added++
		v.meta[p.ID] = p
		for _, f := range p.CompiledGoFiles {
			v.fileIDs[f] = append(v.fileIDs[f], p.ID)
		}
		for _, f := range p.GoFiles {
			if !contains(v.fileIDs[f], p.ID) {
				v.fileIDs[f] = append(v.fileIDs[f], p.ID)
			}
		}
	})
	return added
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// pickID returns the ID of the package with the fewest files among the packages that contain file.
// For a non-test file, this picks the regular package over the variant rebuilt for tests, such as "p [p.test]".
func (v *view) pickID(file string) string {
	v.mu.Lock()
	defer v.mu.Unlock()
	best := ""
	for _, id := range v.fileIDs[file] {
		p := v.meta[id]
		if p == nil {
			continue
		}
		if best == "" || len(p.CompiledGoFiles) < len(v.meta[best].CompiledGoFiles) {
			best = id
		}
	}
	return best
}

func (v *view) metaOf(id string) *packages.Package {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.meta[id]
}

// isImmutable reports whether p and all its dependencies are in GOROOT or in the module cache, so that their content never changes.
// Call it without holding v.mu.
func (v *view) isImmutable(p *packages.Package) bool {
	v.mu.Lock()
	r, ok := v.immutable[p.ID]
	v.mu.Unlock()
	if ok {
		return r
	}
	r = v.selfImmutable(p)
	if r {
		for _, imp := range p.Imports {
			if !v.isImmutable(imp) {
				r = false
				break
			}
		}
	}
	v.mu.Lock()
	v.immutable[p.ID] = r
	v.mu.Unlock()
	return r
}

func (v *view) selfImmutable(p *packages.Package) bool {
	if p.ID != p.PkgPath {
		return false
	}
	if p.PkgPath == "unsafe" {
		return true
	}
	if len(p.GoFiles) == 0 {
		return false
	}
	env := v.goEnv()
	dir := filepath.Dir(p.GoFiles[0])
	if p.Module == nil {
		return env.GOROOT != "" && strings.HasPrefix(dir, env.GOROOT+string(filepath.Separator))
	}
	if p.Module.Main {
		return false
	}
	return env.GOMODCACHE != "" && strings.HasPrefix(dir, env.GOMODCACHE+string(filepath.Separator))
}

// cacheKey returns the key that identifies the type information of the package p.
// Packages with the same key have the same type information, including their transitive dependencies.
// For an immutable package, it is also the key to look up the type information on disk.
// The types that p refers to change with the versions of its dependencies, so the keys of the dependencies are mixed in.
func (v *view) cacheKey(p *packages.Package) string {
	v.mu.Lock()
	k, ok := v.keys[p.ID]
	v.mu.Unlock()
	if ok {
		return k
	}
	env := v.goEnv()
	h := sha256.New()
	mod := "std"
	if p.Module != nil {
		mod = p.Module.Path + "@" + p.Module.Version
		if p.Module.Replace != nil {
			mod += "=>" + p.Module.Replace.Path + "@" + p.Module.Replace.Version
		}
	}
	_, _ = fmt.Fprintf(h, "%s\n%s\n%s\n%s\n%s\n%s\n%s\n", cacheFormat, env.GOVERSION, env.GOOS, env.GOARCH, env.CGO_ENABLED, p.ID, mod)
	for _, f := range p.CompiledGoFiles {
		_, _ = fmt.Fprintf(h, "f %s\n", filepath.Base(f))
	}
	if !v.isImmutable(p) {
		// For a mutable package, mix in the location and the modification time of its files,
		// or the edit version for an open file.
		// The key changes when a file changes, so stale type information is never picked up.
		// The Go version changes the result of type checking, so it is mixed in too.
		goVersion := ""
		if p.Module != nil {
			goVersion = p.Module.GoVersion
		}
		_, _ = fmt.Fprintf(h, "mutable %s\n%s", goVersion, v.stamp(p))
	}
	paths := make([]string, 0, len(p.Imports))
	for path := range p.Imports {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		_, _ = fmt.Fprintf(h, "i %s %s\n", path, v.cacheKey(p.Imports[path]))
	}
	k = hex.EncodeToString(h.Sum(nil))
	v.mu.Lock()
	v.keys[p.ID] = k
	v.mu.Unlock()
	return k
}

// cacheRoot overrides the directory of the type information saved on disk. Tests set it to keep the user cache untouched.
var cacheRoot string

func cachePath(key string) string {
	dir := cacheRoot
	if dir == "" {
		userCache, err := os.UserCacheDir()
		if err != nil {
			userCache = os.TempDir()
		}
		dir = filepath.Join(userCache, "golspk")
	}
	return filepath.Join(dir, "types", key[:2], key)
}

// dep starts type-checking the package id as a dependency and returns the holder of the result.
// The caller waits for done before reading types.
func (v *view) dep(id string) *depPkg {
	v.mu.Lock()
	d, m := v.deps[id], v.meta[id]
	v.mu.Unlock()
	if d != nil {
		return d
	}
	// A package with the same key reuses the type information loaded by another view,
	// so that each view does not hold its own copy.
	// cacheKey mixes in the keys of the dependencies, so the same key means the same type information for all transitive dependencies.
	shared, created := (*depPkg)(nil), false
	if m != nil && m.PkgPath != "unsafe" {
		shared, created = v.s.sharedDep(v.cacheKey(m), !v.isImmutable(m))
	}
	v.mu.Lock()
	if d := v.deps[id]; d != nil {
		v.mu.Unlock()
		// Another goroutine registered the package first.
		// If this call created the shared holder, load it anyway so that the holder is not left unfinished.
		if created {
			go v.buildDep(id, shared)
		}
		return d
	}
	hit := false
	switch {
	case shared == nil:
		d = &depPkg{done: make(chan struct{})}
		go v.buildDep(id, d)
	case created:
		d = shared
		v.sharedMisses++
		go v.buildDep(id, d)
	default:
		d = shared
		v.sharedHits++
		hit = true
	}
	v.deps[id] = d
	v.mu.Unlock()
	if hit {
		// Register the dependencies of a reused package in deps of this view too.
		// The list of loaded packages and the decision to drop a package from the shared pool both look at deps of the views.
		for _, imp := range m.Imports {
			v.dep(imp.ID)
		}
	}
	return d
}

func (v *view) buildDep(id string, d *depPkg) {
	defer close(d.done)
	m := v.metaOf(id)
	if m == nil {
		return
	}
	if m.PkgPath == "unsafe" {
		d.types = types.Unsafe
		return
	}
	imports := v.waitImports(m)

	cpuSem <- struct{}{}
	defer func() { <-cpuSem }()

	immutable := v.isImmutable(m)
	if immutable {
		if tp := v.readExport(m); tp != nil {
			d.types = tp
			return
		}
	}
	files, _, _ := v.parseFiles(m, false)
	tp, _ := v.check(m, files, imports, nil)
	d.types = tp
	if immutable && tp != nil {
		go v.writeExport(m, tp)
	}
}

// waitImports starts type-checking every package that m imports concurrently and waits for all of them.
func (v *view) waitImports(m *packages.Package) map[string]*types.Package {
	deps := make(map[string]*depPkg, len(m.Imports))
	for path, imp := range m.Imports {
		deps[path] = v.dep(imp.ID)
	}
	imports := make(map[string]*types.Package, len(deps))
	for path, d := range deps {
		<-d.done
		if d.types != nil {
			imports[path] = d.types
		}
	}
	return imports
}

func (v *view) readExport(m *packages.Package) *types.Package {
	f, err := os.Open(cachePath(v.cacheKey(m)))
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	// gcexportdata.Read looks up the types of other packages that m refers to in imports,
	// and creates incomplete packages for the ones it does not find.
	// Pass the type information of all transitive dependencies of m so that no incomplete package is created.
	// The dependencies are loaded before m.
	imports := v.importClosure(m)
	// Reading may add content to the packages passed in, so reads are serialized across the whole server.
	v.s.exportMu.Lock()
	defer v.s.exportMu.Unlock()
	tp, err := gcexportdata.Read(bufio.NewReader(f), v.s.fset, imports, m.PkgPath)
	if err != nil {
		logf("readExport %s: %v", m.ID, err)
		return nil
	}
	return tp
}

// importClosure returns the type information of the transitive dependencies of m, keyed by package path.
func (v *view) importClosure(m *packages.Package) map[string]*types.Package {
	out := map[string]*types.Package{}
	seen := map[string]bool{}
	var visit func(p *packages.Package)
	visit = func(p *packages.Package) {
		for _, imp := range p.Imports {
			if seen[imp.ID] {
				continue
			}
			seen[imp.ID] = true
			if tp := v.loadedTypes(imp); tp != nil {
				out[imp.PkgPath] = tp
			}
			visit(imp)
		}
	}
	visit(m)
	return out
}

// loadedTypes returns the loaded type information of p, or nil if loading has not finished.
// It also looks in the shared pool, in case the package has not been registered in deps of this view yet.
func (v *view) loadedTypes(p *packages.Package) *types.Package {
	if p.PkgPath == "unsafe" {
		return types.Unsafe
	}
	v.mu.Lock()
	d := v.deps[p.ID]
	v.mu.Unlock()
	if d == nil && v.isImmutable(p) {
		d = v.s.sharedDepIfExists(v.cacheKey(p))
	}
	if d == nil {
		return nil
	}
	select {
	case <-d.done:
		return d.types
	default:
		return nil
	}
}

func (v *view) writeExport(m *packages.Package, tp *types.Package) {
	var buf bytes.Buffer
	if err := gcexportdata.Write(&buf, v.s.fset, tp); err != nil {
		logf("writeExport %s: %v", m.ID, err)
		return
	}
	path := cachePath(v.cacheKey(m))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "tmp-*")
	if err != nil {
		return
	}
	_, werr := tmp.Write(buf.Bytes())
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		_ = os.Remove(tmp.Name())
		return
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = os.Remove(tmp.Name())
	}
}

// parseFiles parses the Go files of m. When full is true, it keeps comments and also returns the content for position conversion.
func (v *view) parseFiles(m *packages.Package, full bool) ([]*ast.File, map[string]*ast.File, map[string]*mapper) {
	mode := parser.SkipObjectResolution
	if full {
		mode |= parser.ParseComments
	}
	var files []*ast.File
	byName := map[string]*ast.File{}
	mappers := map[string]*mapper{}
	record := !v.selfImmutable(m)
	for _, name := range m.CompiledGoFiles {
		if !strings.HasSuffix(name, ".go") {
			continue
		}
		_, open := v.s.overlayVersion(name)
		stamp, stamped := "", false
		if record && !open {
			stamp, stamped = fileStamp(name)
		}
		src, err := v.s.readFile(name)
		if err != nil {
			continue
		}
		if stamped {
			v.s.recordFileStamp(name, stamp)
		}
		f, _ := parser.ParseFile(v.s.fset, name, src, mode)
		if f == nil {
			continue
		}
		files = append(files, f)
		if full {
			byName[name] = f
			mappers[name] = newMapper(src)
		}
	}
	return files, byName, mappers
}

type importerFunc func(path string) (*types.Package, error)

func (f importerFunc) Import(path string) (*types.Package, error) { return f(path) }

// check type-checks m. When info is nil, it skips function bodies.
// The second result reports whether m imports a package that is not in imports.
func (v *view) check(m *packages.Package, files []*ast.File, imports map[string]*types.Package, info *types.Info) (*types.Package, bool) {
	missing := false
	conf := types.Config{
		Importer: importerFunc(func(path string) (*types.Package, error) {
			if path == "unsafe" {
				return types.Unsafe, nil
			}
			if tp := imports[path]; tp != nil {
				return tp, nil
			}
			missing = true
			return nil, fmt.Errorf("package %q is not loaded", path)
		}),
		IgnoreFuncBodies: info == nil,
		Error:            func(error) {},
		Sizes:            types.SizesFor("gc", v.goEnv().GOARCH),
	}
	if m.Module != nil && m.Module.GoVersion != "" {
		if gv := "go" + m.Module.GoVersion; version.IsValid(gv) {
			conf.GoVersion = gv
		}
	}
	tp, _ := conf.Check(m.PkgPath, v.s.fset, files, info)
	return tp, missing
}

// stamp returns a string that changes when the content of the files of m changes.
func (v *view) stamp(m *packages.Package) string {
	var b strings.Builder
	for _, name := range m.CompiledGoFiles {
		if ver, ok := v.s.overlayVersion(name); ok {
			fmt.Fprintf(&b, "%s o%d\n", name, ver)
			continue
		}
		if st, err := os.Stat(name); err == nil {
			fmt.Fprintf(&b, "%s %d %d\n", name, st.ModTime().UnixNano(), st.Size())
		}
	}
	return b.String()
}

// full returns the package id type-checked with function bodies. It returns the previous result when no file has changed.
func (v *view) full(id string) *fullPkg {
	m := v.metaOf(id)
	if m == nil {
		return nil
	}
	v.mu.Lock()
	e := v.fulls[id]
	if e == nil {
		e = &fullEntry{}
		v.fulls[id] = e
	}
	v.mu.Unlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	stamp := v.stamp(m)
	if e.pkg != nil && e.pkg.stamp == stamp && e.pkg.meta == m {
		return e.pkg
	}
	start := time.Now()
	e.pkg = v.buildFull(m)
	logf("full %s: %v", id, time.Since(start))
	return e.pkg
}

// buildFull type-checks m with function bodies.
func (v *view) buildFull(m *packages.Package) *fullPkg {
	stamp := v.stamp(m)
	imports := v.waitImports(m)

	cpuSem <- struct{}{}
	defer func() { <-cpuSem }()
	files, byName, mappers := v.parseFiles(m, true)
	info := &types.Info{
		Defs:      map[*ast.Ident]types.Object{},
		Uses:      map[*ast.Ident]types.Object{},
		Implicits: map[ast.Node]types.Object{},
	}
	tp, missing := v.check(m, files, imports, info)
	return &fullPkg{view: v, meta: m, stamp: stamp, files: byName, mappers: mappers, types: tp, info: info, missingImport: missing}
}

// invalidate drops the type-checked results of the packages ids and of the packages that depend on them.
func (v *view) invalidate(ids []string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	rev := map[string][]string{}
	for id, p := range v.meta {
		for _, imp := range p.Imports {
			rev[imp.ID] = append(rev[imp.ID], id)
		}
	}
	seen := map[string]bool{}
	queue := append([]string(nil), ids...)
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		delete(v.deps, id)
		delete(v.fulls, id)
		// Recompute the key too, because it includes the modification time of the files.
		delete(v.keys, id)
		queue = append(queue, rev[id]...)
	}
}

// resetMutable drops the metadata and the type-checked results of all mutable packages, such as those of the main module.
// Call it when the metadata is stale because a file was added or removed, or because imports changed.
func (v *view) resetMutable() {
	var mutable []*packages.Package
	v.mu.Lock()
	all := make([]*packages.Package, 0, len(v.meta))
	for _, p := range v.meta {
		all = append(all, p)
	}
	v.mu.Unlock()
	for _, p := range all {
		if !v.isImmutable(p) {
			mutable = append(mutable, p)
		}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, p := range mutable {
		delete(v.meta, p.ID)
		delete(v.deps, p.ID)
		delete(v.fulls, p.ID)
		delete(v.immutable, p.ID)
		delete(v.keys, p.ID)
	}
	for f, ids := range v.fileIDs {
		kept := ids[:0]
		for _, id := range ids {
			if v.meta[id] != nil {
				kept = append(kept, id)
			}
		}
		if len(kept) == 0 {
			delete(v.fileIDs, f)
		} else {
			v.fileIDs[f] = kept
		}
	}
}

// findModRoot walks up from file and returns the first directory that has a go.mod.
func findModRoot(file string) string {
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Dir(file)
		}
		dir = parent
	}
}

func posToken(fset *token.FileSet, pos token.Pos) token.Position {
	return fset.PositionFor(pos, false)
}
