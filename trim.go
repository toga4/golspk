package main

import (
	"path/filepath"
	"runtime/debug"
	"time"
)

// trimDelay is the time without requests after which golspk drops what open files do not need.
const trimDelay = 2 * time.Minute

// trim drops the packages that no open file needs, such as those loaded by a search that spans many packages,
// and the modules that have no open file. Go to definition and hover in open files keep working without loading again.
// A later search loads the dropped packages again, from the type information saved on disk where it can.
func (s *server) trim() {
	if s.inflight.Load() > 0 {
		// A long search is running. The end of the request schedules the next trim.
		return
	}
	start := time.Now()
	s.mu.Lock()
	open := make([]string, 0, len(s.opened))
	for f := range s.opened {
		open = append(open, f)
	}
	views := make(map[string]*view, len(s.views))
	for root, v := range s.views {
		views[root] = v
	}
	s.mu.Unlock()

	// Features answer for an open file from the module that contains it, so only that module keeps what the file needs,
	// even though other modules that depend on the module also have the file in their metadata.
	// A file outside the loaded modules, such as one in the module cache, is answered from a module that has loaded it,
	// so every module keeps it.
	byView := map[string][]string{}
	for _, f := range open {
		if root := findModRoot(f); views[root] != nil {
			byView[root] = append(byView[root], f)
			continue
		}
		for root := range views {
			byView[root] = append(byView[root], f)
		}
	}

	kept, dropped := 0, 0
	for root, v := range views {
		n, ok := v.trim(byView[root])
		kept += n
		if !ok {
			dropped++
			s.mu.Lock()
			if s.views[root] == v {
				delete(s.views, root)
				delete(s.moduleScans, root)
			}
			s.mu.Unlock()
		}
	}
	s.mu.Lock()
	s.parsed = map[string]*parsedFile{}
	s.mu.Unlock()
	s.sweepShared(true)
	s.pruneDisk(open)
	logf("trim: kept %d packages, dropped %d modules in %v", kept, dropped, time.Since(start))
	debug.FreeOSMemory()
}

// trim keeps only the packages of the open files and their transitive dependencies, and returns the number kept.
// It reports false when the view has no open file, so that the caller drops the whole view.
func (v *view) trim(open []string) (int, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	// Start from the package that features use for each open file.
	// A file can also belong to the test variant of its package loaded by a search, which imports far more.
	var queue []string
	for _, f := range open {
		if id := v.pickIDLocked(f); id != "" {
			queue = append(queue, id)
		}
	}
	if len(queue) == 0 {
		return 0, false
	}
	keep := map[string]bool{}
	for len(queue) > 0 {
		id := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if keep[id] {
			continue
		}
		keep[id] = true
		if p := v.meta[id]; p != nil {
			for _, imp := range p.Imports {
				queue = append(queue, imp.ID)
			}
		}
	}
	// Drop the metadata too: the metadata of a package whose files are no longer watched can go stale,
	// and the next request that needs the package loads it again with go list.
	for id := range v.meta {
		if !keep[id] {
			delete(v.meta, id)
			delete(v.deps, id)
			delete(v.fulls, id)
			delete(v.keys, id)
			delete(v.immutable, id)
		}
	}
	for f, ids := range v.fileIDs {
		kept := ids[:0]
		for _, id := range ids {
			if keep[id] {
				kept = append(kept, id)
			}
		}
		if len(kept) == 0 {
			delete(v.fileIDs, f)
		} else {
			v.fileIDs[f] = kept
		}
	}
	return len(v.meta), true
}

// pruneDisk drops the records of the files and directories that no longer belong to a loaded package,
// so that checkDisk does not keep checking the files of dropped packages.
func (s *server) pruneDisk(open []string) {
	if !s.pollDisk.Load() {
		return
	}
	keep := map[string]bool{}
	for _, f := range open {
		keep[f] = true
	}
	s.mu.Lock()
	views := make([]*view, 0, len(s.views))
	for _, v := range s.views {
		views = append(views, v)
	}
	s.mu.Unlock()
	for _, v := range views {
		for _, name := range []string{"go.mod", "go.sum", "go.work"} {
			keep[filepath.Join(v.root, name)] = true
		}
		v.mu.Lock()
		for f := range v.fileIDs {
			keep[f] = true
			keep[filepath.Dir(f)] = true
		}
		v.mu.Unlock()
	}
	s.disk.mu.Lock()
	for path := range s.disk.entries {
		if !keep[path] {
			delete(s.disk.entries, path)
		}
	}
	s.disk.mu.Unlock()
}
