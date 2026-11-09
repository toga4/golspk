package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/tools/go/packages"
)

// moduleScanDepth is the maximum depth to look for go.mod files under a workspace folder.
const moduleScanDepth = 6

type moduleScan struct {
	at    time.Time
	roots []string
}

// setFolders stores the workspace folders found in the parameters of initialize.
func (s *server) setFolders(params json.RawMessage) {
	var p struct {
		RootURI          string `json:"rootUri"`
		WorkspaceFolders []struct {
			URI string `json:"uri"`
		} `json:"workspaceFolders"`
	}
	if json.Unmarshal(params, &p) != nil {
		return
	}
	var folders []string
	for _, f := range p.WorkspaceFolders {
		if path := uriToPath(f.URI); path != "" {
			folders = append(folders, path)
		}
	}
	if len(folders) == 0 {
		if path := uriToPath(p.RootURI); path != "" {
			folders = append(folders, path)
		}
	}
	s.mu.Lock()
	s.folders = folders
	s.mu.Unlock()
}

// workspaceModules returns the directories of the go.mod files in the workspace folders and in the git repository that contains origin.
func (s *server) workspaceModules(origin string) []string {
	s.mu.Lock()
	folders := append([]string(nil), s.folders...)
	s.mu.Unlock()
	if root := gitRoot(origin); root != "" {
		folders = append(folders, root)
	}
	seen := map[string]bool{origin: true}
	roots := []string{origin}
	for _, folder := range folders {
		for _, root := range s.scanModules(folder) {
			if !seen[root] {
				seen[root] = true
				roots = append(roots, root)
			}
		}
	}
	sort.Strings(roots)
	return roots
}

func (s *server) scanModules(folder string) []string {
	s.mu.Lock()
	cached, ok := s.moduleScans[folder]
	s.mu.Unlock()
	if ok && time.Since(cached.at) < time.Minute {
		return cached.roots
	}
	var roots []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			name := e.Name()
			if !e.IsDir() {
				if name == "go.mod" {
					roots = append(roots, dir)
				}
				continue
			}
			if depth >= moduleScanDepth || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "node_modules" || name == "vendor" || name == "testdata" {
				continue
			}
			walk(filepath.Join(dir, name), depth+1)
		}
	}
	walk(folder, 0)
	s.mu.Lock()
	s.moduleScans[folder] = moduleScan{at: time.Now(), roots: roots}
	s.mu.Unlock()
	return roots
}

func gitRoot(dir string) string {
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// searchRoots returns the module directories to search for references to, or implementations of, an object declared in decl.
// When the module of decl is in the workspace, it returns origin and every workspace module whose go.mod mentions that module.
// Almost every module uses the standard library and third-party modules, and loading all of them takes long the first time.
// So for objects declared there, it returns only origin unless all is true.
// The second result reports whether the search was narrowed to origin for this reason.
func (s *server) searchRoots(origin *view, decl *packages.Package, all bool) ([]string, bool) {
	modules := s.workspaceModules(origin.root)
	modPath, declDir := "", ""
	if decl != nil && decl.Module != nil {
		modPath, declDir = decl.Module.Path, decl.Module.Dir
	}
	external := true
	for _, root := range modules {
		if declDir != "" && root == declDir {
			external = false
		}
	}
	if external && !all {
		return []string{origin.root}, len(modules) > 1
	}
	var roots []string
	for _, root := range modules {
		if root == origin.root || root == declDir || modPath == "" {
			roots = append(roots, root)
			continue
		}
		if content, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil && bytes.Contains(content, []byte(modPath)) {
			roots = append(roots, root)
		}
	}
	return roots, false
}

// notifyLimited tells the client that a search was narrowed to the module of the open file.
// The client shows a button that widens the search to all modules.
func (s *server) notifyLimited(method, file string, pos lspPosition, name string) {
	_ = s.conn.notify("golspk/searchLimited", map[string]any{
		"method":   method,
		"uri":      pathToURI(file),
		"position": pos,
		"name":     name,
	})
}

func (s *server) viewByRoot(root string) *view {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.views[root]
	if v == nil {
		v = newView(s, root)
		s.views[root] = v
	}
	return v
}
