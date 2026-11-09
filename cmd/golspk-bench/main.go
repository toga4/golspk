// Command golspk-bench starts an LSP server the way an editor does and measures
// the time until go to definition and hover respond after opening a file.
//
// Usage:
//
//	golspk-bench [-server "golspk"] [-memory] ROOT_DIR FILE LINE COL
//
// LINE and COL are 1-based. The result is one line of JSON on standard output.
// Run it with -server "gopls" to compare against gopls.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/toga4/golspk/internal/lspclient"
)

type result struct {
	Server     string   `json:"server"`
	File       string   `json:"file"`
	Initialize float64  `json:"initializeSeconds"`
	Definition float64  `json:"definitionSeconds"`
	Hover      float64  `json:"hoverSeconds"`
	Locations  []string `json:"definition"`
	HoverOK    bool     `json:"hoverOK"`
	// RSSMB and FootprintMB are set with -memory.
	// RSS on macOS includes memory already returned to the OS. The footprint is what Activity Monitor shows as Memory.
	RSSMB       *int `json:"rssMB,omitempty"`
	FootprintMB *int `json:"footprintMB,omitempty"`
}

func main() {
	server := flag.String("server", "golspk", "command line of the LSP server to measure")
	memory := flag.Bool("memory", false, "also report the memory of the server after it has been idle for -settle")
	settle := flag.Duration("settle", 6*time.Second, "how long to wait before measuring memory")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: golspk-bench [flags] ROOT_DIR FILE LINE COL")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 4 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*server, *memory, *settle, flag.Args()); err != nil {
		fmt.Fprintln(os.Stderr, "golspk-bench:", err)
		os.Exit(1)
	}
}

func run(server string, memory bool, settle time.Duration, args []string) error {
	root, err := filepath.Abs(args[0])
	if err != nil {
		return err
	}
	file, err := filepath.Abs(args[1])
	if err != nil {
		return err
	}
	line, err := strconv.Atoi(args[2])
	if err != nil {
		return fmt.Errorf("LINE: %w", err)
	}
	col, err := strconv.Atoi(args[3])
	if err != nil {
		return fmt.Errorf("COL: %w", err)
	}
	text, err := os.ReadFile(file)
	if err != nil {
		return err
	}

	command := strings.Fields(server)
	if len(command) == 0 {
		return fmt.Errorf("-server is empty")
	}
	start := time.Now()
	cmd := exec.Command(command[0], command[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()

	client := lspclient.New(stdout, stdin)
	client.OnRequest = func(method string, params json.RawMessage) any {
		// gopls asks for its settings. Answer with one empty object per requested item.
		if method == "workspace/configuration" {
			var p struct {
				Items []json.RawMessage `json:"items"`
			}
			_ = json.Unmarshal(params, &p)
			return make([]map[string]any, len(p.Items))
		}
		return nil
	}
	client.Start()
	seconds := func() float64 { return float64(time.Since(start).Milliseconds()) / 1000 }

	rootURI, fileURI := toURI(root), toURI(file)
	if err := client.Call("initialize", map[string]any{
		"processId":        os.Getpid(),
		"rootUri":          rootURI,
		"workspaceFolders": []map[string]any{{"uri": rootURI, "name": filepath.Base(root)}},
		"capabilities": map[string]any{
			"workspace":    map[string]any{"workspaceFolders": true, "configuration": true},
			"textDocument": map[string]any{"hover": map[string]any{"contentFormat": []string{"markdown", "plaintext"}}},
		},
	}, nil); err != nil {
		return err
	}
	res := result{Server: server, File: file, Initialize: seconds()}
	if err := client.Notify("initialized", map[string]any{}); err != nil {
		return err
	}
	if err := client.Notify("textDocument/didOpen", map[string]any{
		"textDocument": map[string]any{"uri": fileURI, "languageId": "go", "version": 1, "text": string(text)},
	}); err != nil {
		return err
	}

	position := map[string]any{
		"textDocument": map[string]any{"uri": fileURI},
		"position":     map[string]any{"line": line - 1, "character": col - 1},
	}
	// The result is a single location, a list of locations or a list of location links, depending on the server.
	var locations json.RawMessage
	if err := client.Call("textDocument/definition", position, &locations); err != nil {
		return err
	}
	res.Definition = seconds()
	res.Locations = describeLocations(locations)

	var hover json.RawMessage
	if err := client.Call("textDocument/hover", position, &hover); err != nil {
		return err
	}
	res.Hover = seconds()
	res.HoverOK = len(hover) > 0 && string(hover) != "null"

	if memory {
		time.Sleep(settle)
		res.RSSMB, res.FootprintMB = measureMemory(cmd.Process.Pid)
	}
	return json.NewEncoder(os.Stdout).Encode(res)
}

func toURI(path string) string {
	return (&url.URL{Scheme: "file", Path: path}).String()
}

// describeLocations formats the locations of a definition response as "path:line" with 1-based lines.
func describeLocations(raw json.RawMessage) []string {
	type location struct {
		URI       string `json:"uri"`
		TargetURI string `json:"targetUri"`
		Range     *struct {
			Start struct {
				Line int `json:"line"`
			} `json:"start"`
		} `json:"range"`
		TargetRange *struct {
			Start struct {
				Line int `json:"line"`
			} `json:"start"`
		} `json:"targetRange"`
	}
	var list []location
	if err := json.Unmarshal(raw, &list); err != nil {
		var one location
		if json.Unmarshal(raw, &one) != nil {
			return nil
		}
		list = []location{one}
	}
	out := []string{}
	for _, l := range list {
		uri, line := l.URI, 0
		if l.Range != nil {
			line = l.Range.Start.Line
		}
		if l.TargetURI != "" {
			uri = l.TargetURI
			if l.TargetRange != nil {
				line = l.TargetRange.Start.Line
			}
		}
		if u, err := url.Parse(uri); err == nil {
			uri = u.Path
		}
		out = append(out, fmt.Sprintf("%s:%d", uri, line+1))
	}
	return out
}

var footprintPattern = regexp.MustCompile(`phys_footprint:\s+([0-9.]+)\s+(KB|MB|GB)`)

// measureMemory returns the RSS of the process, and on macOS also its physical footprint, in megabytes.
func measureMemory(pid int) (rss, footprint *int) {
	if out, err := exec.Command("ps", "-o", "rss=", "-p", strconv.Itoa(pid)).Output(); err == nil {
		if kb, err := strconv.Atoi(strings.TrimSpace(string(out))); err == nil {
			mb := kb / 1024
			rss = &mb
		}
	}
	if runtime.GOOS != "darwin" {
		return rss, nil
	}
	out, err := exec.Command("footprint", strconv.Itoa(pid)).Output()
	if err != nil {
		return rss, nil
	}
	m := footprintPattern.FindSubmatch(out)
	if m == nil {
		return rss, nil
	}
	value, err := strconv.ParseFloat(string(m[1]), 64)
	if err != nil {
		return rss, nil
	}
	switch string(m[2]) {
	case "KB":
		value /= 1024
	case "GB":
		value *= 1024
	}
	mb := int(value)
	return rss, &mb
}
