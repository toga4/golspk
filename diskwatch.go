package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// diskWatch notices changes on disk for clients that do not watch files for the server,
// such as an agent that edits files with its own tools and never sends workspace/didChangeWatchedFiles.
// golspk records the modification time and size of each workspace file it reads, and the Go files of each package directory.
// Before each request, it compares the records with the disk and handles the differences as watcher notifications.
type diskWatch struct {
	mu      sync.Mutex
	entries map[string]*diskEntry
}

type diskEntry struct {
	stamp string
	// names are the Go files in a directory. It is nil for a file.
	names map[string]bool
}

// recordFile records the current state on disk of a file.
func (s *server) recordFile(path string) {
	if !s.pollDisk.Load() {
		return
	}
	if stamp, ok := fileStamp(path); ok {
		s.recordFileStamp(path, stamp)
	}
}

// recordFileStamp records stamp as the state on disk of a file.
// Take the stamp before reading the file, so that an edit during the read is noticed by the next check.
func (s *server) recordFileStamp(path, stamp string) {
	if !s.pollDisk.Load() {
		return
	}
	s.disk.mu.Lock()
	s.disk.entries[path] = &diskEntry{stamp: stamp}
	s.disk.mu.Unlock()
}

// recordNewFile records the state on disk of a file, unless the file is already recorded.
func (s *server) recordNewFile(path string) {
	s.disk.mu.Lock()
	_, ok := s.disk.entries[path]
	s.disk.mu.Unlock()
	if !ok {
		s.recordFile(path)
	}
}

// recordDir records the Go files of a package directory, unless the directory is already recorded.
func (s *server) recordDir(dir string) {
	if !s.pollDisk.Load() {
		return
	}
	s.disk.mu.Lock()
	_, ok := s.disk.entries[dir]
	s.disk.mu.Unlock()
	if ok {
		return
	}
	stamp, ok := fileStamp(dir)
	if !ok {
		return
	}
	e := &diskEntry{stamp: stamp, names: goFileNames(dir)}
	s.disk.mu.Lock()
	if s.disk.entries[dir] == nil {
		s.disk.entries[dir] = e
	}
	s.disk.mu.Unlock()
}

func goFileNames(dir string) map[string]bool {
	names := map[string]bool{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			names[e.Name()] = true
		}
	}
	return names
}

// checkDisk compares the recorded files and directories with the disk,
// and drops the loaded results that depend on the ones that changed.
func (s *server) checkDisk() {
	if !s.pollDisk.Load() {
		return
	}
	type change struct {
		path string
		kind int
	}
	var changes []change
	start := time.Now()
	s.disk.mu.Lock()
	n := len(s.disk.entries)
	for path, e := range s.disk.entries {
		stamp, ok := fileStamp(path)
		switch {
		case !ok:
			delete(s.disk.entries, path)
			// The files of a removed directory are reported one by one, because each of them is recorded too.
			if e.names == nil {
				changes = append(changes, change{path, 3})
			}
		case stamp == e.stamp:
		case e.names != nil:
			// A removed file is reported by its own record, so only look for added files.
			e.stamp = stamp
			names := goFileNames(path)
			for name := range names {
				if !e.names[name] {
					changes = append(changes, change{filepath.Join(path, name), 1})
				}
			}
			e.names = names
		default:
			e.stamp = stamp
			changes = append(changes, change{path, 2})
		}
	}
	s.disk.mu.Unlock()
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		logf("checkDisk: %d files and directories in %v", n, elapsed)
	}
	if len(changes) == 0 {
		return
	}
	// Such a client does not report its later edits of an open file either, so the content on disk is newer than the content it sent.
	s.mu.Lock()
	for _, c := range changes {
		delete(s.overlays, c.path)
	}
	s.mu.Unlock()
	logf("changes on disk: %d files", len(changes))
	s.filesChanged(func(yield func(string, int)) {
		for _, c := range changes {
			yield(c.path, c.kind)
		}
	})
}
