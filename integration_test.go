package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/toga4/golspk/internal/lspclient"
)

func TestMain(m *testing.M) {
	// Share one cache of type information between the tests, and keep the cache of the user untouched.
	dir, err := os.MkdirTemp("", "golspk-test-cache-")
	if err != nil {
		panic(err)
	}
	cacheRoot = dir
	flag.Parse()
	if !testing.Verbose() {
		logOutput = io.Discard
	}
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// session is a server running against a temporary copy of testdata/implmod.
type session struct {
	t      *testing.T
	root   string
	client *lspclient.Client

	mu            sync.Mutex
	notifications map[string][]json.RawMessage
}

// startSession copies testdata/implmod to a temporary directory and starts a server on it.
func startSession(t *testing.T, initializationOptions any) *session {
	t.Helper()
	// Resolve symlinks, because the temporary directory on macOS is one and go list reports resolved paths.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(root, os.DirFS("testdata/implmod")); err != nil {
		t.Fatal(err)
	}
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	srv := newServer(newConn(serverReader, serverWriter))
	go func() { _ = srv.run() }()
	s := &session{t: t, root: root, client: lspclient.New(clientReader, clientWriter), notifications: map[string][]json.RawMessage{}}
	s.client.OnNotification = func(method string, params json.RawMessage) {
		s.mu.Lock()
		s.notifications[method] = append(s.notifications[method], params)
		s.mu.Unlock()
	}
	s.client.Start()
	t.Cleanup(func() {
		_ = clientWriter.Close()
		_ = serverWriter.Close()
	})
	s.call("initialize", map[string]any{
		"rootUri":               pathToURI(root),
		"capabilities":          map[string]any{},
		"initializationOptions": initializationOptions,
	}, nil)
	s.notify("initialized", map[string]any{})
	return s
}

func (s *session) call(method string, params, result any) {
	s.t.Helper()
	if err := s.client.Call(method, params, result); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) notify(method string, params any) {
	s.t.Helper()
	if err := s.client.Notify(method, params); err != nil {
		s.t.Fatal(err)
	}
}

func (s *session) path(rel string) string { return filepath.Join(s.root, rel) }

func (s *session) read(rel string) string {
	s.t.Helper()
	b, err := os.ReadFile(s.path(rel))
	if err != nil {
		s.t.Fatal(err)
	}
	return string(b)
}

// open sends the content of the file on disk as an opened document.
func (s *session) open(rel string) {
	s.t.Helper()
	s.notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(s.path(rel)), "languageId": "go", "version": 1, "text": s.read(rel)},
	})
}

// write rewrites a file on disk, or removes it when content is empty, and sends the file watcher notification.
func (s *session) write(rel, content string) {
	s.t.Helper()
	_, statErr := os.Stat(s.path(rel))
	kind := 2
	if content == "" {
		kind = 3
		if err := os.Remove(s.path(rel)); err != nil {
			s.t.Fatal(err)
		}
	} else {
		if statErr != nil {
			kind = 1
		}
		if err := os.WriteFile(s.path(rel), []byte(content), 0o644); err != nil {
			s.t.Fatal(err)
		}
	}
	s.notify("workspace/didChangeWatchedFiles", map[string]any{
		"changes": []map[string]any{{"uri": pathToURI(s.path(rel)), "type": kind}},
	})
}

// at returns the request parameters for the position delta characters after the start of needle in the file.
func (s *session) at(rel, needle string, delta int) map[string]any {
	s.t.Helper()
	text := s.read(rel)
	offset := strings.Index(text, needle)
	if offset < 0 {
		s.t.Fatalf("%q is not in %s", needle, rel)
	}
	offset += delta
	line := strings.Count(text[:offset], "\n")
	col := offset - (strings.LastIndex(text[:offset], "\n") + 1)
	return map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(s.path(rel))},
		"position":     lspPosition{Line: line, Character: col},
	}
}

// rel formats a location as "path:line" with the path relative to the workspace and a 1-based line.
func (s *session) rel(uri string, r lspRange) string {
	path, err := filepath.Rel(s.root, uriToPath(uri))
	if err != nil {
		path = uri
	}
	return fmt.Sprintf("%s:%d", filepath.ToSlash(path), r.Start.Line+1)
}

// locations calls a method that returns locations and formats them with rel, sorted.
func (s *session) locations(method string, params map[string]any) []string {
	s.t.Helper()
	var locs []lspLocation
	s.call(method, params, &locs)
	out := make([]string, 0, len(locs))
	for _, l := range locs {
		out = append(out, s.rel(l.URI, l.Range))
	}
	slices.Sort(out)
	return out
}

func (s *session) hover(params map[string]any) string {
	s.t.Helper()
	var h *hoverResult
	s.call("textDocument/hover", params, &h)
	if h == nil {
		return ""
	}
	return h.Contents.Value
}

// hierarchy prepares a hierarchy item at the position and returns the names of the items that method returns, sorted.
func (s *session) hierarchy(prepare, method string, params map[string]any) []string {
	s.t.Helper()
	var items []hierarchyItem
	s.call(prepare, params, &items)
	if len(items) != 1 {
		s.t.Fatalf("%s returned %d items, want 1", prepare, len(items))
	}
	var raw []json.RawMessage
	s.call(method, map[string]any{"item": items[0]}, &raw)
	names := []string{}
	for _, r := range raw {
		// Call hierarchy results wrap the item in "from" or "to". Type hierarchy results are the items themselves.
		var entry struct {
			hierarchyItem
			From *hierarchyItem `json:"from"`
			To   *hierarchyItem `json:"to"`
		}
		if err := json.Unmarshal(r, &entry); err != nil {
			s.t.Fatal(err)
		}
		switch {
		case entry.From != nil:
			names = append(names, entry.From.Name)
		case entry.To != nil:
			names = append(names, entry.To.Name)
		default:
			names = append(names, entry.Name)
		}
	}
	slices.Sort(names)
	return names
}

func (s *session) symbols(query string) []string {
	s.t.Helper()
	var syms []workspaceSymbol
	s.call("workspace/symbol", map[string]any{"query": query}, &syms)
	names := []string{}
	for _, sym := range syms {
		names = append(names, sym.Name)
	}
	return names
}

func (s *session) received(method string) []json.RawMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.notifications[method])
}

func equal(t *testing.T, label string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s = %v, want %v", label, got, want)
	}
}

func TestNavigation(t *testing.T) {
	s := startSession(t, nil)
	s.open("d/d.go")
	newBase := s.at("d/d.go", "NewBase(", 1)

	equal(t, "definition", s.locations("textDocument/definition", newBase), []string{"a/a.go:16"})
	equal(t, "definition of an imported package", s.locations("textDocument/definition", s.at("d/d.go", `"example.com/implmod/a"`, 3)), []string{"a/a.go:1"})
	equal(t, "typeDefinition", s.locations("textDocument/typeDefinition", newBase), []string{"a/a.go:11"})
	var std []lspLocation
	s.call("textDocument/definition", s.at("d/d.go", "errors.New(", 8), &std)
	if len(std) != 1 || !strings.HasSuffix(std[0].URI, "/src/errors/errors.go") {
		t.Errorf("definition in the standard library = %v, want one location in errors/errors.go", std)
	}

	hover := s.hover(newBase)
	for _, want := range []string{"func a.NewBase(name string) a.Base", "NewBase returns a Base."} {
		if !strings.Contains(hover, want) {
			t.Errorf("hover = %q, want it to contain %q", hover, want)
		}
	}
	if hover := s.hover(s.at("d/d.go", "errors.New(", 8)); !strings.Contains(hover, "func errors.New(text string) error") || !strings.Contains(hover, "pkg.go.dev/errors#New") {
		t.Errorf("hover of a standard library function = %q", hover)
	}
	if hover := s.hover(s.at("d/d.go", "error {", 1)); !strings.Contains(hover, "type error interface") {
		t.Errorf("hover of a built-in type = %q", hover)
	}

	withDecl := s.at("d/d.go", "NewBase(", 1)
	withDecl["context"] = map[string]any{"includeDeclaration": true}
	equal(t, "references", s.locations("textDocument/references", withDecl), []string{"a/a.go:16", "d/d.go:14"})
	equal(t, "references without the declaration", s.locations("textDocument/references", newBase), []string{"d/d.go:14"})

	// Base appears in its declaration, as a receiver, as a result type and in a composite literal.
	s.open("a/a.go")
	var highlights []documentHighlight
	s.call("textDocument/documentHighlight", s.at("a/a.go", "Base struct", 1), &highlights)
	if len(highlights) != 4 {
		t.Errorf("documentHighlight returned %d ranges, want 4", len(highlights))
	}

	var outline []documentSymbol
	s.call("textDocument/documentSymbol", map[string]any{"textDocument": map[string]any{"uri": pathToURI(s.path("a/a.go"))}}, &outline)
	names := []string{}
	for _, sym := range outline {
		names = append(names, sym.Name)
	}
	equal(t, "documentSymbol", names, []string{"Runner", "Getter", "Base", "(Base).RunSomethingLong", "NewBase"})
}

func TestImplementations(t *testing.T) {
	s := startSession(t, nil)
	s.open("a/a.go")
	runner := s.at("a/a.go", "Runner interface", 1)

	// Base has the method itself, Embedder and ViaInterface get it through embedding, Box is generic, and mockRunner is in a test file.
	equal(t, "implementations of an interface", s.locations("textDocument/implementation", runner),
		[]string{"a/a.go:11", "b/b.go:11", "b/b.go:6", "c/c.go:4", "c/c_test.go:3"})
	equal(t, "implementations of an interface method", s.locations("textDocument/implementation", s.at("a/a.go", "RunSomethingLong(n int) error\n}", 1)),
		[]string{"a/a.go:13", "c/c.go:6", "c/c_test.go:5"})
	equal(t, "implementations of a generic interface", s.locations("textDocument/implementation", s.at("a/a.go", "Getter[T any]", 1)),
		[]string{"c/c.go:11", "c/c.go:4"})

	s.open("c/c.go")
	equal(t, "interfaces of a generic type", s.locations("textDocument/implementation", s.at("c/c.go", "Box[T any]", 1)),
		[]string{"a/a.go:3", "a/a.go:7"})
	equal(t, "interface methods of a method", s.locations("textDocument/implementation", s.at("c/c.go", "GetValue() int", 1)),
		[]string{"a/a.go:8"})

	equal(t, "subtypes", s.hierarchy("textDocument/prepareTypeHierarchy", "typeHierarchy/subtypes", runner),
		[]string{"Base", "Box", "Embedder", "ViaInterface", "mockRunner"})
	equal(t, "supertypes", s.hierarchy("textDocument/prepareTypeHierarchy", "typeHierarchy/supertypes", s.at("c/c.go", "Box[T any]", 1)),
		[]string{"Getter", "Runner"})
	equal(t, "subtypes of a concrete type", s.hierarchy("textDocument/prepareTypeHierarchy", "typeHierarchy/subtypes", s.at("c/c.go", "Box[T any]", 1)),
		[]string{})
}

func TestCallHierarchy(t *testing.T) {
	s := startSession(t, nil)
	s.open("d/d.go")
	equal(t, "incoming calls", s.hierarchy("textDocument/prepareCallHierarchy", "callHierarchy/incomingCalls", s.at("d/d.go", "NewBase(", 1)), []string{"Run"})
	equal(t, "outgoing calls", s.hierarchy("textDocument/prepareCallHierarchy", "callHierarchy/outgoingCalls", s.at("d/d.go", "func Run", 6)),
		[]string{"Base.RunSomethingLong", "NewBase"})
}

func TestWorkspaceSymbol(t *testing.T) {
	s := startSession(t, nil)
	equal(t, "exact name", s.symbols("NewBase"), []string{"NewBase"})
	equal(t, "method by type and name", s.symbols("Box.Get"), []string{"Box.GetValue"})
	// Prefix matches come before the weak matches, where the characters only appear in order.
	if got := s.symbols("err"); len(got) < 2 || !slices.Equal(got[:2], []string{"ErrD", "ErrOther"}) {
		t.Errorf("case-insensitive prefix = %v, want it to start with ErrD and ErrOther", got)
	}
	equal(t, "empty query", s.symbols(""), []string{})
}

func TestSearchOptions(t *testing.T) {
	s := startSession(t, map[string]any{"excludeTests": true})
	s.open("a/a.go")
	subtypes := func() []string {
		return s.hierarchy("textDocument/prepareTypeHierarchy", "typeHierarchy/subtypes", s.at("a/a.go", "Runner interface", 1))
	}
	equal(t, "subtypes without tests", subtypes(), []string{"Base", "Box", "Embedder", "ViaInterface"})
	equal(t, "symbols without tests", s.symbols("mockRunner"), []string{})

	s.notify("golspk/setOptions", map[string]any{"exclude": []string{"b", "**/c_test.go"}})
	equal(t, "subtypes with exclude globs", subtypes(), []string{"Base", "Box"})

	s.notify("golspk/setOptions", map[string]any{"exclude": []string{"{b,c}/**"}})
	equal(t, "subtypes with a brace glob", subtypes(), []string{"Base"})

	s.notify("golspk/setOptions", map[string]any{})
	equal(t, "subtypes with everything included", subtypes(), []string{"Base", "Box", "Embedder", "ViaInterface", "mockRunner"})
}

func TestSearchAllModules(t *testing.T) {
	s := startSession(t, nil)
	s.open("d/d.go")
	errorsNew := s.at("d/d.go", "errors.New(", 8)

	// A symbol of the standard library is searched only in the module of the open file, and the server reports that.
	equal(t, "references in the module", s.locations("textDocument/references", errorsNew), []string{"d/d.go:10"})
	limited := s.received("golspk/searchLimited")
	if len(limited) != 1 {
		t.Fatalf("got %d golspk/searchLimited notifications, want 1", len(limited))
	}
	var params struct {
		Method string `json:"method"`
		Name   string `json:"name"`
	}
	if err := json.Unmarshal(limited[0], &params); err != nil {
		t.Fatal(err)
	}
	if params.Method != "references" || params.Name != "New" {
		t.Errorf("golspk/searchLimited = %+v, want references of New", params)
	}

	equal(t, "references in all modules", s.locations("golspk/references", errorsNew), []string{"d/d.go:10", "other/other.go:7"})

	// A symbol declared in the workspace is searched in every module that mentions its module, without a report.
	equal(t, "references to a workspace symbol", s.locations("textDocument/references", s.at("d/d.go", "NewBase(", 1)), []string{"d/d.go:14"})
	if n := len(s.received("golspk/searchLimited")); n != 1 {
		t.Errorf("got %d golspk/searchLimited notifications after searching a workspace symbol, want 1", n)
	}
}

func TestFollowsFileChanges(t *testing.T) {
	s := startSession(t, nil)
	s.open("d/d.go")
	newBase := s.at("d/d.go", "NewBase(", 1)
	if hover := s.hover(newBase); !strings.Contains(hover, "name string") {
		t.Fatalf("hover = %q", hover)
	}
	original := s.read("a/a.go")

	// A changed dependency.
	s.write("a/a.go", strings.Replace(original, "func NewBase(name string)", "func NewBase(renamed string)", 1))
	if hover := s.hover(newBase); !strings.Contains(hover, "renamed string") {
		t.Errorf("hover after changing a dependency = %q", hover)
	}

	// Lines added to a dependency.
	s.write("a/a.go", strings.Replace(original, "package a\n", "package a\n\n// line 1\n// line 2\n", 1))
	equal(t, "definition after adding lines", s.locations("textDocument/definition", newBase), []string{"a/a.go:19"})

	// A file added to a dependency and used from the open file through an unsaved edit.
	s.write("a/added.go", "package a\n\n// Added is added while the server is running.\nfunc Added() string { return \"\" }\n")
	edited := s.read("d/d.go") + "\nfunc callAdded() string { return a.Added() }\n"
	s.notify("textDocument/didChange", map[string]any{
		"textDocument":   map[string]any{"uri": pathToURI(s.path("d/d.go")), "version": 2},
		"contentChanges": []map[string]any{{"text": edited}},
	})
	line := strings.Count(edited, "\n") - 1
	added := map[string]any{
		"textDocument": map[string]any{"uri": pathToURI(s.path("d/d.go"))},
		"position":     lspPosition{Line: line, Character: strings.Index("func callAdded() string { return a.Added() }", "a.Added()") + 3},
	}
	equal(t, "definition in an added file", s.locations("textDocument/definition", added), []string{"a/added.go:4"})
	equal(t, "symbol of an added file", s.symbols("Added"), []string{"Added"})

	// A removed file. The symbol index is refreshed from the watcher notification.
	s.write("a/added.go", "")
	equal(t, "symbol of a removed file", s.symbols("Added"), []string{})

	// A reverted dependency.
	s.write("a/a.go", original)
	if hover := s.hover(newBase); !strings.Contains(hover, "name string") {
		t.Errorf("hover after reverting a dependency = %q", hover)
	}
}

func TestResponseTime(t *testing.T) {
	// The first request after opening a file must not load more than the package of the file and its dependencies.
	// This is a loose bound that only catches a regression to loading the whole module or to blocking forever.
	s := startSession(t, nil)
	start := time.Now()
	s.open("d/d.go")
	s.locations("textDocument/definition", s.at("d/d.go", "NewBase(", 1))
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("the first go to definition took %v", elapsed)
	}
}

func TestScanSymbols(t *testing.T) {
	src := `package p

import "fmt"

const A, B = 1, 2

const (
	C = iota
	D
)

var E = func() { type inner struct{}; fmt.Println() }

var (
	F int
	G, H string
)

type I interface{ M() }

type (
	J struct{ field int }
	K[T any] []T
	L = J
)

type N func(a int) error

func O() {}

func (j *J) P() {}

func (k K[T]) Q() {}

func R[T any](v T) T { return v }
`
	got := []string{}
	for _, sym := range scanSymbols("p.go", []byte(src)) {
		got = append(got, fmt.Sprintf("%d %s", sym.kind, sym.name))
	}
	want := []string{
		"14 A", "14 B", "14 C", "14 D",
		"13 E", "13 F", "13 G", "13 H",
		"11 I", "23 J", "5 K", "5 L", "5 N",
		"12 O", "6 J.P", "6 K.Q", "12 R",
	}
	equal(t, "scanSymbols", got, want)
}
