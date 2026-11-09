package main

import (
	"net/url"
	"unicode/utf8"
)

// mapper converts between LSP positions (line and UTF-16 column) and byte offsets in the content of a file.
type mapper struct {
	content    []byte
	lineStarts []int
}

func newMapper(content []byte) *mapper {
	starts := []int{0}
	for i, b := range content {
		if b == '\n' {
			starts = append(starts, i+1)
		}
	}
	return &mapper{content: content, lineStarts: starts}
}

// offset returns the byte offset of the 0-based line and the UTF-16 column char.
func (m *mapper) offset(line, char int) int {
	if line < 0 {
		return 0
	}
	if line >= len(m.lineStarts) {
		return len(m.content)
	}
	off := m.lineStarts[line]
	end := len(m.content)
	if line+1 < len(m.lineStarts) {
		end = m.lineStarts[line+1]
	}
	units := 0
	for off < end && units < char {
		r, size := utf8.DecodeRune(m.content[off:end])
		if r == '\n' {
			break
		}
		units += utf16Len(r)
		off += size
	}
	return off
}

// position returns the line and the UTF-16 column of the byte offset off.
func (m *mapper) position(off int) lspPosition {
	off = max(0, min(off, len(m.content)))
	lo, hi := 0, len(m.lineStarts)
	for hi-lo > 1 {
		mid := (lo + hi) / 2
		if m.lineStarts[mid] <= off {
			lo = mid
		} else {
			hi = mid
		}
	}
	units := 0
	for i := m.lineStarts[lo]; i < off; {
		r, size := utf8.DecodeRune(m.content[i:off])
		units += utf16Len(r)
		i += size
	}
	return lspPosition{Line: lo, Character: units}
}

// lineColOffset returns the byte offset of the 1-based line and the 1-based byte column col.
func (m *mapper) lineColOffset(line, col int) int {
	if line < 1 || line > len(m.lineStarts) {
		return len(m.content)
	}
	return min(m.lineStarts[line-1]+col-1, len(m.content))
}

func utf16Len(r rune) int {
	if r >= 0x10000 {
		return 2
	}
	return 1
}

type lspPosition struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start lspPosition `json:"start"`
	End   lspPosition `json:"end"`
}

type lspLocation struct {
	URI   string   `json:"uri"`
	Range lspRange `json:"range"`
}

func uriToPath(uri string) string {
	u, err := url.Parse(uri)
	if err != nil || u.Scheme != "file" {
		return ""
	}
	return u.Path
}

func pathToURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}
