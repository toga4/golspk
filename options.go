package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
)

// options holds the settings that the client can change.
type options struct {
	// Exclude lists doublestar globs of paths to leave out of searches. They match paths relative to a workspace folder.
	Exclude []string `json:"exclude"`
	// ExcludeTests reports whether searches leave out _test.go files.
	ExcludeTests bool `json:"excludeTests"`
	// TrimAfterMinutes is the time without requests after which the server drops what open files do not need.
	// Nil means defaultTrimAfter, and zero or less turns trimming off.
	TrimAfterMinutes *float64 `json:"trimAfterMinutes"`
	// UnloadAfterMinutes is the time without requests after which the server drops everything it has loaded.
	// Nil means defaultUnloadAfter, and zero or less turns unloading off.
	UnloadAfterMinutes *float64 `json:"unloadAfterMinutes"`
}

// idleDelay returns the delay that minutes sets, or def when minutes is nil. It returns zero when minutes turns the work off.
func idleDelay(minutes *float64, def time.Duration) time.Duration {
	if minutes == nil {
		return def
	}
	if *minutes <= 0 {
		return 0
	}
	return time.Duration(*minutes * float64(time.Minute))
}

// setOptions stores the settings carried by initializationOptions of initialize or by a golspk/setOptions notification.
func (s *server) setOptions(raw json.RawMessage) {
	if len(raw) == 0 || string(raw) == "null" {
		return
	}
	var o options
	if err := json.Unmarshal(raw, &o); err != nil {
		logf("setOptions: %v", err)
		return
	}
	for _, pat := range o.Exclude {
		if !doublestar.ValidatePattern(pat) {
			logf("setOptions: invalid glob %q", pat)
		}
	}
	s.mu.Lock()
	s.opts = o
	s.mu.Unlock()
	logf("setOptions: exclude %q, excludeTests %v, trim after %v, unload after %v", o.Exclude, o.ExcludeTests,
		idleDelay(o.TrimAfterMinutes, defaultTrimAfter), idleDelay(o.UnloadAfterMinutes, defaultUnloadAfter))
	// Start the timers again, so that new delays apply without waiting for a request.
	s.scheduleRelease()
}

// skipPath reports whether searches (references, implementations, hierarchies and symbols) leave out path.
// Go to definition and hover do not use it.
func (s *server) skipPath(path string) bool {
	s.mu.Lock()
	o := s.opts
	bases := s.folders
	s.mu.Unlock()
	if o.ExcludeTests && strings.HasSuffix(path, "_test.go") {
		return true
	}
	if len(o.Exclude) == 0 {
		return false
	}
	rel := strings.TrimPrefix(path, string(filepath.Separator))
	for _, base := range bases {
		if strings.HasPrefix(path, base+string(filepath.Separator)) {
			rel = path[len(base)+1:]
			break
		}
	}
	rel = filepath.ToSlash(rel)
	for _, pat := range o.Exclude {
		// A pattern that names a directory matches every file under it.
		if ok, _ := doublestar.Match(pat, rel); ok {
			return true
		}
		if ok, _ := doublestar.Match(strings.TrimSuffix(pat, "/")+"/**", rel); ok {
			return true
		}
	}
	return false
}

// filterLocations removes the locations in paths that searches leave out.
func (s *server) filterLocations(locs []lspLocation) []lspLocation {
	out := locs[:0]
	for _, l := range locs {
		if !s.skipPath(uriToPath(l.URI)) {
			out = append(out, l)
		}
	}
	return out
}
