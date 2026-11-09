# golspk for Claude Code

This plugin runs [golspk](../README.md) as the Go language server of Claude Code.

## Install

Install the server with the Go toolchain, and make sure that `$(go env GOPATH)/bin` is in your `PATH`.

```bash
go install github.com/toga4/golspk@latest
```

Then install the plugin at the prompt of Claude Code 2.1.275 or later.

```
/plugin install golspk --marketplace toga4/golspk
```

With an earlier version, add the marketplace first.

```
/plugin marketplace add toga4/golspk
/plugin install golspk@golspk
```

The gopls plugin of the official marketplace also handles `.go` files. If you have installed it, turn it off so that golspk answers.

```bash
claude plugin disable gopls-lsp@claude-plugins-official
```

## What Claude gets

Claude Code gives Claude an `LSP` tool for go to definition, find references, hover, go to implementations, document symbols, workspace symbols and call hierarchy.
golspk answers all of them.
Claude decides on its own when to call the tool. To make it prefer the tool over a text search, say so in your prompt or in `CLAUDE.md`.

golspk returns no diagnostics, and the plugin turns off diagnostics after edits.
With the gopls plugin, Claude sees type errors right after each edit. With golspk, Claude sees them only when it runs `go build` or `go vet` on the changed packages.
No message from the language server does not mean that the code compiles.

Claude Code sends the content of the files that Claude edits with its Edit and Write tools.
It does not report changes made by shell commands, so golspk compares the modification times of the files it has read with the disk before each request.

golspk drops what open files do not need after 2 minutes without requests, and everything it has loaded after 10 minutes, as described in [Memory](../README.md#memory).
This keeps the memory of idle sessions low when several Claude Code sessions run at the same time.

Claude Code does not start plugin language servers in cloud sessions, so golspk works only in sessions on your machine.

## Update

The plugin has no version, so Claude Code takes its version from the commit of this repository and picks up new commits when it updates the plugin.
Auto-update is off for marketplaces other than the official ones. Turn it on in the Marketplaces tab of `/plugin`, or update the plugin by hand.

```bash
claude plugin update golspk@golspk
```

The plugin does not include the server. Run `go install github.com/toga4/golspk@latest` again to update it.
