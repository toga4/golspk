package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/token"
	"go/types"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// serverVersion is the version of golspk. It matches the version of the extension.
const serverVersion = "0.1.0"

// logOutput is where logf writes. Tests replace it.
var logOutput io.Writer = os.Stderr

func logf(format string, args ...any) {
	_, _ = fmt.Fprintf(logOutput, time.Now().Format("15:04:05.000")+" "+format+"\n", args...)
}

type overlay struct {
	version int
	text    []byte
}

type server struct {
	conn *conn
	fset *token.FileSet

	mu       sync.Mutex
	overlays map[string]*overlay
	views    map[string]*view
	parsed   map[string]*parsedFile
	retried  map[string]string
	// folders are the workspace folders that the client has open.
	folders     []string
	moduleScans map[string]moduleScan
	opts        options
	symbols     *symbolIndex
	// lang is the display language of the client, a key of messages.
	lang string

	// shared is the pool that shares the type information of packages between views, keyed by cacheKey.
	sharedMu sync.Mutex
	shared   map[string]*depPkg
	// exportMu serializes the reads of the type information saved on disk.
	exportMu sync.Mutex

	releaseTimer *time.Timer
}

// sharedDepIfExists returns the holder of the package whose cacheKey is key, if there is one.
func (s *server) sharedDepIfExists(key string) *depPkg {
	s.sharedMu.Lock()
	defer s.sharedMu.Unlock()
	return s.shared[key]
}

// loadedDeps returns the type information of every package that has finished loading:
// the dependencies of v and the packages shared between views.
func (s *server) loadedDeps(v *view) []*types.Package {
	seen := map[*depPkg]bool{}
	var list []*depPkg
	v.mu.Lock()
	for _, d := range v.deps {
		if !seen[d] {
			seen[d] = true
			list = append(list, d)
		}
	}
	v.mu.Unlock()
	s.sharedMu.Lock()
	for _, d := range s.shared {
		if !seen[d] {
			seen[d] = true
			list = append(list, d)
		}
	}
	s.sharedMu.Unlock()
	var out []*types.Package
	for _, d := range list {
		select {
		case <-d.done:
			if d.types != nil {
				out = append(out, d.types)
			}
		default:
		}
	}
	return out
}

// releaseDelay is the time between the last response and the release of unused memory to the OS.
const releaseDelay = 3 * time.Second

// scheduleRelease releases unused memory to the OS when no request arrives for releaseDelay.
// A search that type-checks many packages uses a lot of memory for a short time,
// and the Go runtime does not return it to the OS right away.
func (s *server) scheduleRelease() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.releaseTimer != nil {
		s.releaseTimer.Stop()
	}
	s.releaseTimer = time.AfterFunc(releaseDelay, debug.FreeOSMemory)
}

// memStats runs a garbage collection and returns the size of the live heap. It is used for measurements.
func (s *server) memStats() map[string]any {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	s.sharedMu.Lock()
	shared := len(s.shared)
	s.sharedMu.Unlock()
	s.mu.Lock()
	views := len(s.views)
	s.mu.Unlock()
	return map[string]any{"heapAllocMB": m.HeapAlloc >> 20, "sysMB": m.Sys >> 20, "views": views, "sharedPackages": shared}
}

// sharedDep returns the holder of the package whose cacheKey is key.
// The second result reports whether the holder was newly created, in which case the caller loads the package.
func (s *server) sharedDep(key string, mutable bool) (*depPkg, bool) {
	s.sharedMu.Lock()
	defer s.sharedMu.Unlock()
	if d := s.shared[key]; d != nil {
		return d, false
	}
	d := &depPkg{done: make(chan struct{}), mutable: mutable}
	s.shared[key] = d
	return d, true
}

// sweepShared drops from the shared pool the type information of the mutable packages that no view uses.
// The key changes when a file changes, and the type information under the old key would otherwise stay forever.
func (s *server) sweepShared() {
	s.mu.Lock()
	views := make([]*view, 0, len(s.views))
	for _, v := range s.views {
		views = append(views, v)
	}
	s.mu.Unlock()
	used := map[*depPkg]bool{}
	for _, v := range views {
		v.mu.Lock()
		for _, d := range v.deps {
			used[d] = true
		}
		v.mu.Unlock()
	}
	s.sharedMu.Lock()
	defer s.sharedMu.Unlock()
	for key, d := range s.shared {
		if d.mutable && !used[d] {
			delete(s.shared, key)
		}
	}
}

func newServer(c *conn) *server {
	return &server{
		conn:        c,
		fset:        token.NewFileSet(),
		overlays:    map[string]*overlay{},
		views:       map[string]*view{},
		parsed:      map[string]*parsedFile{},
		retried:     map[string]string{},
		moduleScans: map[string]moduleScan{},
		symbols:     &symbolIndex{files: map[string]*fileSymbols{}},
		shared:      map[string]*depPkg{},
		lang:        "en",
	}
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("golspk v" + serverVersion)
		return
	}
	s := newServer(newConn(os.Stdin, os.Stdout))
	if err := s.run(); !errors.Is(err, io.EOF) {
		logf("exit: %v", err)
		os.Exit(1)
	}
}

func (s *server) run() error {
	for {
		req, err := s.conn.read()
		if err != nil {
			return err
		}
		switch {
		case req.Method == "":
			// A response to a request sent by the server. The result is not used.
		case req.ID == nil:
			// Handle notifications in the order received, so that a later request reads the latest content of open files.
			s.notify(req)
		default:
			go s.handle(req)
		}
	}
}

func (s *server) readFile(name string) ([]byte, error) {
	s.mu.Lock()
	o := s.overlays[name]
	s.mu.Unlock()
	if o != nil {
		return o.text, nil
	}
	return os.ReadFile(name)
}

func (s *server) overlayVersion(name string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o := s.overlays[name]; o != nil {
		return o.version, true
	}
	return 0, false
}

// viewFor returns the view that handles file.
// It is the view of the module that contains file, the one with the nearest go.mod.
// A file in the module cache or in GOROOT is handled by a view that has loaded its package as a dependency,
// so that dependency versions match those of the main module the user jumped from.
func (s *server) viewFor(file string) *view {
	root := findModRoot(file)
	s.mu.Lock()
	own := s.views[root]
	views := make([]*view, 0, len(s.views))
	for _, v := range s.views {
		views = append(views, v)
	}
	s.mu.Unlock()
	if own != nil {
		return own
	}
	for _, v := range views {
		env := v.goEnv()
		external := (env.GOMODCACHE != "" && strings.HasPrefix(file, env.GOMODCACHE+string(filepath.Separator))) ||
			(env.GOROOT != "" && strings.HasPrefix(file, env.GOROOT+string(filepath.Separator)))
		if !external {
			continue
		}
		v.mu.Lock()
		_, ok := v.fileIDs[file]
		v.mu.Unlock()
		if ok {
			return v
		}
	}
	return s.viewByRoot(root)
}

// packageFor returns the package that contains file, type-checked with function bodies.
func (s *server) packageFor(file string) (*fullPkg, error) {
	v := s.viewFor(file)
	id := v.pickID(file)
	if id == "" {
		v.loadMu.Lock()
		if id = v.pickID(file); id == "" {
			if err := v.loadMeta(file); err != nil {
				v.loadMu.Unlock()
				return nil, err
			}
			id = v.pickID(file)
		}
		v.loadMu.Unlock()
		if id == "" {
			return nil, fmt.Errorf("no package found for %s", file)
		}
	}
	p := v.full(id)
	if p == nil {
		return nil, fmt.Errorf("no package found for %s", file)
	}
	if !p.missingImport {
		return p, nil
	}
	// An import that the metadata lacks may have just been added, so load the metadata again, but only once.
	key := v.root + "\x00" + id
	s.mu.Lock()
	done := s.retried[key] == p.stamp
	s.retried[key] = p.stamp
	s.mu.Unlock()
	if done {
		return p, nil
	}
	v.resetMutable()
	if err := v.loadMeta(file); err != nil {
		return p, nil
	}
	if id = v.pickID(file); id == "" {
		return p, nil
	}
	if np := v.full(id); np != nil {
		s.mu.Lock()
		s.retried[v.root+"\x00"+id] = np.stamp
		s.mu.Unlock()
		return np, nil
	}
	return p, nil
}

type textDocumentPositionParams struct {
	TextDocument struct {
		URI string `json:"uri"`
	} `json:"textDocument"`
	Position lspPosition `json:"position"`
}

func (s *server) notify(req *request) {
	switch req.Method {
	case "initialized":
		s.registerWatchers()
	case "golspk/setOptions":
		s.setOptions(req.Params)
	case "exit":
		os.Exit(0)
	case "textDocument/didOpen":
		var p struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Version int    `json:"version"`
				Text    string `json:"text"`
			} `json:"textDocument"`
		}
		if json.Unmarshal(req.Params, &p) != nil {
			return
		}
		file := uriToPath(p.TextDocument.URI)
		if file == "" {
			return
		}
		s.mu.Lock()
		s.overlays[file] = &overlay{version: p.TextDocument.Version, text: []byte(p.TextDocument.Text)}
		s.mu.Unlock()
		if strings.HasSuffix(file, ".go") {
			// Start type checking when the file is opened, so that the first go to definition or hover does not wait.
			go func() {
				if _, err := s.packageFor(file); err != nil {
					logf("didOpen %s: %v", file, err)
				}
			}()
		}
	case "textDocument/didChange":
		var p struct {
			TextDocument struct {
				URI     string `json:"uri"`
				Version int    `json:"version"`
			} `json:"textDocument"`
			ContentChanges []struct {
				Text string `json:"text"`
			} `json:"contentChanges"`
		}
		if json.Unmarshal(req.Params, &p) != nil || len(p.ContentChanges) == 0 {
			return
		}
		file := uriToPath(p.TextDocument.URI)
		s.mu.Lock()
		s.overlays[file] = &overlay{version: p.TextDocument.Version, text: []byte(p.ContentChanges[len(p.ContentChanges)-1].Text)}
		s.mu.Unlock()
		// Reflect unsaved edits in the type-checked results of the packages that depend on the package of this file.
		s.filesChanged(func(yield func(string, int)) { yield(file, 2) })
	case "textDocument/didClose":
		var p textDocumentPositionParams
		if json.Unmarshal(req.Params, &p) != nil {
			return
		}
		file := uriToPath(p.TextDocument.URI)
		s.mu.Lock()
		delete(s.overlays, file)
		s.mu.Unlock()
		// Closing without saving reverts the content to the one on disk,
		// so drop the type-checked results of the packages that depend on it.
		s.filesChanged(func(yield func(string, int)) { yield(file, 2) })
	case "workspace/didChangeWatchedFiles":
		var p struct {
			Changes []struct {
				URI  string `json:"uri"`
				Type int    `json:"type"`
			} `json:"changes"`
		}
		if json.Unmarshal(req.Params, &p) != nil {
			return
		}
		s.filesChanged(func(yield func(string, int)) {
			for _, c := range p.Changes {
				yield(uriToPath(c.URI), c.Type)
			}
		})
	}
}

func (s *server) registerWatchers() {
	type watcher struct {
		GlobPattern string `json:"globPattern"`
	}
	_ = s.conn.call("client/registerCapability", map[string]any{
		"registrations": []map[string]any{{
			"id":     "golspk-watch",
			"method": "workspace/didChangeWatchedFiles",
			"registerOptions": map[string]any{
				"watchers": []watcher{{"**/*.go"}, {"**/go.mod"}, {"**/go.sum"}, {"**/go.work"}},
			},
		}},
	})
}

// filesChanged drops the loaded results that are stale because of the files changed on disk.
// kind is the FileChangeType of LSP: 1 for created, 2 for changed and 3 for deleted.
func (s *server) filesChanged(each func(yield func(path string, kind int))) {
	s.mu.Lock()
	views := make([]*view, 0, len(s.views))
	for _, v := range s.views {
		views = append(views, v)
	}
	s.mu.Unlock()

	var touched []string
	dropAll := false
	changed := map[*view][]string{}
	reset := map[*view]bool{}
	each(func(path string, kind int) {
		if strings.HasSuffix(path, ".go") {
			touched = append(touched, path)
		}
		switch filepath.Base(path) {
		case "go.mod", "go.sum", "go.work":
			dropAll = true
			return
		}
		for _, v := range views {
			v.mu.Lock()
			ids := append([]string(nil), v.fileIDs[path]...)
			v.mu.Unlock()
			switch {
			case kind == 2 && len(ids) > 0:
				changed[v] = append(changed[v], ids...)
			case len(ids) > 0 || strings.HasPrefix(path, v.root+string(filepath.Separator)):
				// Adding or removing a file changes the file list of a package, so start over from the metadata.
				reset[v] = true
			}
		}
	})
	if s.symbols.built() {
		// This also runs for notifications of unsaved edits, but the index is built from the content on disk,
		// so a file with an unchanged modification time is not read again.
		for _, path := range touched {
			s.symbols.update(path)
		}
	}
	if dropAll {
		s.mu.Lock()
		s.views = map[string]*view{}
		s.moduleScans = map[string]moduleScan{}
		s.mu.Unlock()
		s.sweepShared()
		return
	}
	for v := range reset {
		v.resetMutable()
	}
	for v, ids := range changed {
		if !reset[v] {
			v.invalidate(ids)
		}
	}
	if len(reset) > 0 || len(changed) > 0 {
		s.sweepShared()
	}
}

func (s *server) handle(req *request) {
	start := time.Now()
	result, rerr := s.dispatch(req)
	s.scheduleRelease()
	if err := s.conn.reply(req.ID, result, rerr); err != nil {
		logf("reply: %v", err)
	}
	if req.Method != "initialize" && req.Method != "shutdown" {
		logf("%s: %v", req.Method, time.Since(start))
	}
}

func (s *server) dispatch(req *request) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		s.setFolders(req.Params)
		var init struct {
			Locale                string          `json:"locale"`
			InitializationOptions json.RawMessage `json:"initializationOptions"`
		}
		if json.Unmarshal(req.Params, &init) == nil {
			s.setLocale(init.Locale)
			s.setOptions(init.InitializationOptions)
		}
		return map[string]any{
			"capabilities": map[string]any{
				"positionEncoding":          "utf-16",
				"textDocumentSync":          map[string]any{"openClose": true, "change": 1},
				"definitionProvider":        true,
				"typeDefinitionProvider":    true,
				"hoverProvider":             true,
				"documentSymbolProvider":    true,
				"documentHighlightProvider": true,
				"referencesProvider":        true,
				"implementationProvider":    true,
				"callHierarchyProvider":     true,
				"typeHierarchyProvider":     true,
				"workspaceSymbolProvider":   true,
			},
			"serverInfo": map[string]any{"name": "golspk", "version": serverVersion},
		}, nil
	case "shutdown":
		return nil, nil
	case "textDocument/references", "golspk/references", "golspk/implementation":
		// golspk/references and golspk/implementation are custom requests sent by the search-all-modules commands of the extension.
		var p struct {
			textDocumentPositionParams
			Context struct {
				IncludeDeclaration bool `json:"includeDeclaration"`
			} `json:"context"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		file := uriToPath(p.TextDocument.URI)
		var result []lspLocation
		var err error
		switch req.Method {
		case "textDocument/references":
			result, err = s.references(file, p.Position, p.Context.IncludeDeclaration, false)
		case "golspk/references":
			result, err = s.references(file, p.Position, p.Context.IncludeDeclaration, true)
		case "golspk/implementation":
			result, err = s.implementation(file, p.Position, true)
		}
		if err != nil {
			logf("%s %s: %v", req.Method, file, err)
			return nil, nil
		}
		return result, nil
	case "golspk/memStats":
		return s.memStats(), nil
	case "workspace/symbol":
		var p struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		return s.workspaceSymbol(p.Query), nil
	case "textDocument/prepareCallHierarchy", "textDocument/prepareTypeHierarchy":
		var p textDocumentPositionParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		file := uriToPath(p.TextDocument.URI)
		var result []hierarchyItem
		var err error
		if req.Method == "textDocument/prepareCallHierarchy" {
			result, err = s.prepareCallHierarchy(file, p.Position)
		} else {
			result, err = s.prepareTypeHierarchy(file, p.Position)
		}
		if err != nil {
			logf("%s %s: %v", req.Method, file, err)
			return nil, nil
		}
		return result, nil
	case "callHierarchy/incomingCalls", "callHierarchy/outgoingCalls", "typeHierarchy/supertypes", "typeHierarchy/subtypes":
		var p struct {
			Item hierarchyItem `json:"item"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		var result any
		var err error
		switch req.Method {
		case "callHierarchy/incomingCalls":
			result, err = s.incomingCalls(p.Item)
		case "callHierarchy/outgoingCalls":
			result, err = s.outgoingCalls(p.Item)
		case "typeHierarchy/supertypes":
			result, err = s.typeHierarchy(p.Item, dirInterfaces)
		case "typeHierarchy/subtypes":
			result, err = s.typeHierarchy(p.Item, dirSubtypes)
		}
		if err != nil {
			logf("%s %s: %v", req.Method, p.Item.URI, err)
			return nil, nil
		}
		return result, nil
	case "textDocument/definition", "textDocument/typeDefinition", "textDocument/implementation", "textDocument/hover", "textDocument/documentHighlight", "textDocument/documentSymbol":
		var p textDocumentPositionParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return nil, &rpcError{Code: -32602, Message: err.Error()}
		}
		file := uriToPath(p.TextDocument.URI)
		if file == "" {
			return nil, nil
		}
		var result any
		var err error
		switch req.Method {
		case "textDocument/definition":
			result, err = s.definition(file, p.Position)
		case "textDocument/typeDefinition":
			result, err = s.typeDefinition(file, p.Position)
		case "textDocument/implementation":
			result, err = s.implementation(file, p.Position, false)
		case "textDocument/hover":
			result, err = s.hover(file, p.Position)
		case "textDocument/documentHighlight":
			result, err = s.documentHighlight(file, p.Position)
		case "textDocument/documentSymbol":
			result, err = s.documentSymbol(file)
		}
		if err != nil {
			logf("%s %s: %v", req.Method, file, err)
			return nil, nil
		}
		return result, nil
	}
	return nil, &rpcError{Code: -32601, Message: "method not found: " + req.Method}
}
