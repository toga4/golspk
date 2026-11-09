# Changelog

## Unreleased

- The server notices changes on disk by itself for a client that does not watch files for it, such as an agent that edits files with its own tools.
- After 2 minutes without requests, the server drops the packages that open files do not need, such as those loaded by a search across many packages.
- After 10 minutes without requests, the server drops everything it has loaded.

## 0.1.1

- The extension is bundled into a single file, so VS Code loads fewer files when it activates the extension.
- The README describes how to install the extension from the Marketplace.

## 0.1.0

The first release.

- Go to definition, go to type definition, and hover with the signature and the doc comment.
- Go to implementations, including types that implement an interface through embedding, generic types and types in test files.
- Find references, which also searches the other modules of the workspace.
- Call hierarchy and type hierarchy.
- Workspace symbols, outline, and highlight of the same identifier.
- The setting `golspk.exclude` and the status bar button for test files, which narrow searches.
- Messages in English and Japanese.
