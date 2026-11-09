package main

import (
	"fmt"
	"go/scanner"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// maxSymbolResults is the maximum number of results of a workspace symbol search.
const maxSymbolResults = 300

// minStrongSymbolResults is the number of results above which weak matches are not added.
const minStrongSymbolResults = 20

// symbol is a declaration in the index of the workspace symbol search.
type symbol struct {
	name  string // a method is named "Type.Method"
	lower string
	pkg   string
	kind  int
	rng   lspRange
}

type fileSymbols struct {
	stamp   string
	symbols []symbol
}

// symbolIndex is an index of the package-level symbols and the methods declared in the Go files of the workspace.
// It is built with the scanner only, without type checking and without go list.
type symbolIndex struct {
	mu     sync.Mutex
	files  map[string]*fileSymbols
	synced time.Time
	// syncMu makes sure that only one rebuild of the index runs at a time.
	syncMu sync.Mutex
}

type workspaceSymbol struct {
	Name          string      `json:"name"`
	Kind          int         `json:"kind"`
	ContainerName string      `json:"containerName,omitempty"`
	Location      lspLocation `json:"location"`
}

// symbolRoots returns the directories to collect symbols from:
// the workspace folders and the git repositories that contain the loaded modules.
func (s *server) symbolRoots() []string {
	s.mu.Lock()
	roots := append([]string(nil), s.folders...)
	for root := range s.views {
		if g := gitRoot(root); g != "" {
			roots = append(roots, g)
		} else {
			roots = append(roots, root)
		}
	}
	s.mu.Unlock()
	sort.Strings(roots)
	// Drop the directories nested in another one, so that no directory is walked twice.
	var out []string
	for _, r := range roots {
		if len(out) > 0 && (r == out[len(out)-1] || strings.HasPrefix(r, out[len(out)-1]+string(filepath.Separator))) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func fileStamp(path string) (string, bool) {
	st, err := os.Stat(path)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%d %d", st.ModTime().UnixNano(), st.Size()), true
}

// sync rebuilds the index from the files on disk.
// It does not read again the files whose modification time and size are unchanged.
func (x *symbolIndex) sync(s *server) {
	x.syncMu.Lock()
	defer x.syncMu.Unlock()
	start := time.Now()
	paths := make(chan string, 256)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for path := range paths {
				x.update(path)
			}
		}()
	}
	seen := map[string]bool{}
	for _, root := range s.symbolRoots() {
		walkGoFiles(root, false, func(path string) {
			seen[path] = true
			paths <- path
		})
	}
	close(paths)
	wg.Wait()
	x.mu.Lock()
	for path := range x.files {
		if !seen[path] {
			delete(x.files, path)
		}
	}
	x.synced = time.Now()
	n := len(x.files)
	x.mu.Unlock()
	logf("symbol index: %d files in %v", n, time.Since(start))
}

// update reads the symbols of path again, or removes them from the index if the file is gone.
func (x *symbolIndex) update(path string) {
	stamp, ok := fileStamp(path)
	x.mu.Lock()
	old := x.files[path]
	if !ok {
		delete(x.files, path)
	}
	x.mu.Unlock()
	if !ok || (old != nil && old.stamp == stamp) {
		return
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return
	}
	fs := &fileSymbols{stamp: stamp, symbols: scanSymbols(path, src)}
	x.mu.Lock()
	x.files[path] = fs
	x.mu.Unlock()
}

// built reports whether the index has been built at least once.
func (x *symbolIndex) built() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return !x.synced.IsZero()
}

type tok struct {
	t   token.Token
	lit string
	off int
}

// scanSymbols extracts the package-level functions, types, variables, constants and the methods declared in src,
// using the scanner only.
// It is faster than parsing because it builds no syntax tree and skips everything inside braces
// (function bodies and the contents of structs).
func scanSymbols(path string, src []byte) []symbol {
	fset := token.NewFileSet()
	file := fset.AddFile(path, -1, len(src))
	var sc scanner.Scanner
	sc.Init(file, src, nil, 0)
	var toks []tok
	brace := 0
	for {
		pos, t, lit := sc.Scan()
		if t == token.EOF {
			break
		}
		switch t {
		case token.LBRACE:
			if brace == 0 {
				toks = append(toks, tok{t: t})
			}
			brace++
			continue
		case token.RBRACE:
			if brace > 0 {
				brace--
			}
			if brace == 0 {
				toks = append(toks, tok{t: t})
			}
			continue
		}
		if brace == 0 {
			toks = append(toks, tok{t: t, lit: lit, off: file.Offset(pos)})
		}
	}

	var m *mapper
	var out []symbol
	pkg := ""
	add := func(name string, kind int, id tok) {
		if id.lit == "_" {
			return
		}
		if m == nil {
			m = newMapper(src)
		}
		out = append(out, symbol{
			name:  name,
			lower: strings.ToLower(name),
			pkg:   pkg,
			kind:  kind,
			rng:   lspRange{Start: m.position(id.off), End: m.position(id.off + len(id.lit))},
		})
	}
	n := len(toks)
	at := func(i int) token.Token {
		if i < n {
			return toks[i].t
		}
		return token.EOF
	}
	// spec extracts the symbols of one declaration of the keyword kw, whose name is toks[j].
	spec := func(kw token.Token, j int) {
		if at(j) != token.IDENT {
			return
		}
		if kw != token.TYPE {
			kind := kindVariable
			if kw == token.CONST {
				kind = kindConstant
			}
			add(toks[j].lit, kind, toks[j])
			for at(j+1) == token.COMMA && at(j+2) == token.IDENT {
				j += 2
				add(toks[j].lit, kind, toks[j])
			}
			return
		}
		k := j + 1
		if at(k) == token.LBRACK {
			// Skip type parameters such as [T any] or an array length such as [3].
			for d := 0; k < n; k++ {
				if toks[k].t == token.LBRACK {
					d++
				} else if toks[k].t == token.RBRACK {
					d--
					if d == 0 {
						k++
						break
					}
				}
			}
		}
		if at(k) == token.ASSIGN {
			k++
		}
		kind := kindClass
		switch at(k) {
		case token.STRUCT:
			kind = kindStruct
		case token.INTERFACE:
			kind = kindInterface
		}
		add(toks[j].lit, kind, toks[j])
	}

	depth := 0
	for i := 0; i < n; i++ {
		switch toks[i].t {
		case token.LPAREN, token.LBRACK:
			depth++
			continue
		case token.RPAREN, token.RBRACK:
			depth--
			continue
		}
		// Handle a declaration keyword only when it is outside parentheses and at the start of a statement,
		// so that a func inside a type or an expression is not taken for a declaration.
		if depth != 0 || (i > 0 && toks[i-1].t != token.SEMICOLON) {
			continue
		}
		switch kw := toks[i].t; kw {
		case token.PACKAGE:
			if at(i+1) == token.IDENT {
				pkg = toks[i+1].lit
			}
		case token.FUNC:
			j := i + 1
			recv := ""
			if at(j) == token.LPAREN {
				// The type name of a receiver such as (r *T[K]) is the last identifier outside brackets.
				for d, br := 0, 0; j < n; j++ {
					switch toks[j].t {
					case token.LPAREN:
						d++
					case token.RPAREN:
						d--
					case token.LBRACK:
						br++
					case token.RBRACK:
						br--
					case token.IDENT:
						if br == 0 {
							recv = toks[j].lit
						}
					}
					if d == 0 {
						break
					}
				}
				j++
			}
			if at(j) != token.IDENT {
				continue
			}
			if recv != "" {
				add(recv+"."+toks[j].lit, kindMethod, toks[j])
			} else {
				add(toks[j].lit, kindFunction, toks[j])
			}
		case token.TYPE, token.VAR, token.CONST:
			j := i + 1
			if at(j) != token.LPAREN {
				spec(kw, j)
				continue
			}
			// In a parenthesized declaration, the identifier right after the opening parenthesis or a semicolon is a name.
			specStart := false
			for d, br := 0, 0; j < n; j++ {
				switch toks[j].t {
				case token.LPAREN:
					d++
					if d == 1 {
						specStart = true
						continue
					}
				case token.RPAREN:
					d--
				case token.LBRACK:
					br++
				case token.RBRACK:
					br--
				case token.SEMICOLON:
					if d == 1 && br == 0 {
						specStart = true
						continue
					}
				}
				if d == 0 {
					break
				}
				if specStart {
					specStart = false
					spec(kw, j)
				}
			}
		}
	}
	return out
}

// symbolScore returns how well the lowercased query q matches sym. Zero means no match.
func symbolScore(q string, sym *symbol) int {
	seg := sym.lower
	if i := strings.LastIndexByte(seg, '.'); i >= 0 {
		seg = seg[i+1:]
	}
	switch {
	case sym.lower == q || seg == q:
		return 5
	case strings.HasPrefix(seg, q) || strings.HasPrefix(sym.lower, q):
		return 4
	case strings.Contains(sym.lower, q):
		return 3
	case strings.Contains(q, ".") && strings.Contains(strings.ToLower(sym.pkg)+"."+sym.lower, q):
		// A query of the form "package.Name".
		return 2
	case isSubsequence(q, sym.lower):
		return 1
	}
	return 0
}

func isSubsequence(q, s string) bool {
	i := 0
	for j := 0; i < len(q) && j < len(s); j++ {
		if q[i] == s[j] {
			i++
		}
	}
	return i == len(q)
}

// workspaceSymbol returns the symbols of all modules in the workspace whose names match query.
func (s *server) workspaceSymbol(query string) []workspaceSymbol {
	x := s.symbols
	x.mu.Lock()
	never, stale := x.synced.IsZero(), time.Since(x.synced) > time.Minute
	x.mu.Unlock()
	if never {
		x.sync(s)
	} else if stale {
		// Rebuild in the background, so that the index does not stay stale when file watcher notifications are missed.
		go x.sync(s)
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return []workspaceSymbol{}
	}
	type hit struct {
		path  string
		sym   *symbol
		score int
	}
	var hits []hit
	x.mu.Lock()
	for path, fs := range x.files {
		for i := range fs.symbols {
			if score := symbolScore(q, &fs.symbols[i]); score > 0 {
				hits = append(hits, hit{path: path, sym: &fs.symbols[i], score: score})
			}
		}
	}
	x.mu.Unlock()
	sort.Slice(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if len(a.sym.name) != len(b.sym.name) {
			return len(a.sym.name) < len(b.sym.name)
		}
		if a.sym.name != b.sym.name {
			return a.sym.name < b.sym.name
		}
		return a.path < b.path
	})
	out := []workspaceSymbol{}
	skip := map[string]bool{}
	for _, h := range hits {
		// Return the weak matches, where the characters only appear in order, only when there are few other matches.
		// This keeps unrelated names out of the results of a long query.
		if len(out) >= maxSymbolResults || (h.score == 1 && len(out) >= minStrongSymbolResults) {
			break
		}
		sk, ok := skip[h.path]
		if !ok {
			sk = s.skipPath(h.path)
			skip[h.path] = sk
		}
		if sk {
			continue
		}
		out = append(out, workspaceSymbol{Name: h.sym.name, Kind: h.sym.kind, ContainerName: h.sym.pkg, Location: lspLocation{URI: pathToURI(h.path), Range: h.sym.rng}})
	}
	return out
}
