# golspk

golspk is a fast, read-only Go language server for reading code, with a VS Code extension.
The name stands for a Go LSP server to peek at code.

gopls loads the whole module with `go list ./...` when it starts, and does not answer go to definition until that finishes.
In a large module this takes several seconds every time you open a new checkout or worktree.
golspk loads nothing when it starts. When you open a file, it loads only the package of that file and its dependencies.
In a module with about 2,000 packages, go to definition and hover respond 2 to 4 seconds after opening a file in a fresh worktree.

golspk does not replace gopls for writing code. It has no completion, diagnostics, formatting, rename or code actions.

## Features

| Feature | LSP method |
|---|---|
| Go to definition | `textDocument/definition` |
| Go to type definition | `textDocument/typeDefinition` |
| Hover with the signature and the doc comment | `textDocument/hover` |
| Go to implementations | `textDocument/implementation` |
| Find references | `textDocument/references` |
| Call hierarchy | `textDocument/prepareCallHierarchy`, `callHierarchy/incomingCalls`, `callHierarchy/outgoingCalls` |
| Type hierarchy | `textDocument/prepareTypeHierarchy`, `typeHierarchy/supertypes`, `typeHierarchy/subtypes` |
| Workspace symbols | `workspace/symbol` |
| Outline and breadcrumbs | `textDocument/documentSymbol` |
| Highlight of the same identifier | `textDocument/documentHighlight` |

The messages of the server and the extension are available in English and Japanese.

## Install

The extension is not published to the Marketplace yet. Build it from source. The packaging script builds for macOS on Apple silicon.

```bash
cd vscode && npm install && npm run package
```

```bash
code --install-extension vscode/golspk-darwin-arm64-0.1.0.vsix
```

Turn off gopls of the Go extension (golang.go) in the VS Code settings.
VS Code waits for the responses of every registered language server before it goes to a definition.
While gopls is running, golspk answers first but VS Code still waits for the initial load of gopls.
With gopls off, the Go extension no longer provides completion, diagnostics, rename or formatting through gopls.
Its test runner and debugger keep working.

```json
{
  "go.useLanguageServer": false
}
```

To use the server with another editor, build it with `go build` and start `golspk` as an LSP server over standard input and output.
The editor must send `workspace/didChangeWatchedFiles`, because golspk relies on it to notice changes on disk.

## Narrowing searches

Two settings narrow the searches for references, implementations, call and type hierarchies, and symbols.
They do not affect go to definition or hover.

- The setting `golspk.exclude` lists globs of paths to leave out. The globs use the [doublestar](https://github.com/bmatcuk/doublestar) syntax, with `**`, `{a,b}` and `[a-z]`. They match paths relative to a workspace folder, and a pattern that names a directory leaves out every file under it.
- The button "Tests: included / excluded" on the right of the status bar toggles whether searches include `_test.go` files. The choice is remembered per workspace.

```json
{
  "golspk.exclude": ["**/generated/**", "**/*_gen.go"]
}
```

## Searching other modules

When the workspace has several modules, find references and go to implementations also search the other modules.

- For a symbol declared in a module of the workspace, golspk searches every workspace module whose go.mod mentions that module.
- For a symbol declared in the standard library or a third-party module, golspk searches only the module of the open file, because almost every module uses such symbols and loading all of them takes long. The extension then shows a button on the status bar for 60 seconds that widens the search to all modules and shows the results in the peek view.
- The commands "golspk: Find References in All Modules" and "golspk: Find Implementations in All Modules" in the editor context menu and the command palette run the wide search directly.

## How it works

- When a file is opened, golspk loads the metadata of its package and dependencies with `go list -deps`. This neither type-checks nor compiles, so it finishes within a second even with more than 1,000 dependencies.
- Dependencies are type-checked from source without function bodies. The build cache is not used, so a change in the source of a dependency never waits for compilation.
- Packages of the standard library and the module cache never change, so their type information is saved in the user cache directory (`~/Library/Caches/golspk` on macOS) and reused by later runs and other checkouts.
- Only the packages of open files are type-checked with function bodies.
- Dependency versions differ between modules, so each module resolves packages on its own. Packages whose transitive dependencies have the same versions share the same type information between modules. Packages of the workspace are shared when the location and the modification time of their files are also the same.
- When a file changes, the type information of its package and of the packages that depend on it is dropped. Type information that no module uses any more is dropped from the shared pool too.
- Memory used for a short time by a search that type-checks many packages is returned to the OS when no request arrives for 3 seconds.
- Find references narrows candidate files with a text search over the sources of the module and type-checks only the candidate packages. A single search type-checks at most 400 packages per module and warns when it stops there.
- Go to implementations looks at the candidate packages narrowed by a text search for method names and at the packages already loaded, which include the standard library and third-party modules.
- Types are matched by method names and signature shapes instead of `types.Implements`. A type parameter matches any type, and named types are compared by package path and name. This matches generic types, types of packages rebuilt for tests and types loaded in other modules.
- Types that get their methods only through struct embedding are found by searching again for files that contain the name of the interface or of a type found so far, up to 64 names.
- Incoming calls of the call hierarchy group the results of find references by the enclosing function. Outgoing calls come from the call expressions in the function body.
- Workspace symbols come from an index built by scanning every Go file in the workspace with the Go scanner, without type checking. The index is built on the first search and updated from file watcher notifications. It covers package-level functions, types, variables, constants and methods, but not struct fields.
- Changes on disk arrive through `workspace/didChangeWatchedFiles`.

## Limitations

- Find references on an interface method does not return the calls made directly on implementing methods, and vice versa.
- A type parameter matches any type, so go to implementations may return combinations that do not satisfy the type constraints.
- When an interface has hundreds of implementations, some types that implement it only through embedding may be missed.
- Types declared inside function bodies are not found by go to implementations.
- A newly added import resolves only after the file is saved, because `go list` reads the files on disk.
- Call and type hierarchies never widen the search for standard library and third-party symbols to other modules.

## Development

```bash
go test ./... && cd vscode && npm test
```

- `go test` starts the server in process against a temporary copy of `testdata/implmod` and checks the results of every request, including how the server follows changes on disk.
- `npm test` checks the translations, builds the server and runs `vscode/test/run.js --fixture`, which launches VS Code with a separate user data directory and checks the same features through the extension.
- `node vscode/test/run.js WORKSPACE_DIR FILE LINE COL` measures the time until go to definition and hover respond through the extension.
- `go run ./cmd/golspk-bench ROOT_DIR FILE LINE COL` measures the same over LSP directly, with the sequence initialize, didOpen, definition and hover. Pass `-server "gopls"` to compare against gopls, and `-memory` to report the memory of the server. RSS reported by `ps` on macOS includes memory already returned to the OS, so the command also reports the physical footprint, which is what Activity Monitor shows.

## License

[MIT](LICENSE)
