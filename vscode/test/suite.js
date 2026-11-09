const fs = require("node:fs");
const path = require("node:path");
const vscode = require("vscode");

const log = (message) => console.log(`[golspk test] ${message}`);

/** Returns the position of needle in the document, offset by delta characters. */
function find(doc, needle, delta = 0) {
	const offset = doc.getText().indexOf(needle);
	if (offset < 0) {
		throw new Error(`${needle} is not in ${doc.uri.fsPath}`);
	}
	return doc.positionAt(offset + delta);
}

/** Repeats fn until its result satisfies ok, for at most 15 seconds. Returns the last result and the elapsed time. */
async function until(fn, ok) {
	const start = Date.now();
	let value = await fn();
	while (!ok(value) && Date.now() - start < 15000) {
		await new Promise((resolve) => setTimeout(resolve, 100));
		value = await fn();
	}
	return { value, ok: ok(value), seconds: ((Date.now() - start) / 1000).toFixed(2) };
}

const run = (command, ...args) => vscode.commands.executeCommand(command, ...args);

async function hoverText(uri, pos) {
	const hovers = await run("vscode.executeHoverProvider", uri, pos);
	return hovers.flatMap((h) => h.contents.map((c) => (typeof c === "string" ? c : c.value))).join("\n");
}

async function definitions(uri, pos) {
	const defs = await run("vscode.executeDefinitionProvider", uri, pos);
	return defs.map((d) => `${vscode.workspace.asRelativePath(d.targetUri ?? d.uri)}:${(d.targetRange ?? d.range).start.line + 1}`);
}

/** Measures the time until go to definition and hover respond at the given position. */
async function measure(uri, pos) {
	const start = Date.now();
	const elapsed = () => `${((Date.now() - start) / 1000).toFixed(2)}s`;
	// Before the extension finishes starting, there is no provider and the result is empty, so repeat until a result arrives.
	const defs = await until(() => definitions(uri, pos), (d) => d.length > 0);
	log(`definition: ${elapsed()} ${defs.value.join(", ")}`);
	const text = await hoverText(uri, pos);
	log(`hover: ${elapsed()} ${JSON.stringify(text.slice(0, 120))}`);
	const symbols = (await run("vscode.executeDocumentSymbolProvider", uri)) ?? [];
	log(`documentSymbol: ${elapsed()} ${symbols.map((s) => s.name).join(", ")}`);
	if (!defs.ok || text === "") {
		throw new Error("go to definition or hover returned nothing");
	}
}

/** Checks every feature against the copy of testdata/implmod opened as the workspace. */
async function checkFixture(uri) {
	const root = vscode.workspace.workspaceFolders[0].uri.fsPath;
	const failures = [];
	const check = (label, ok, detail = "") => {
		log(`${ok ? "OK" : "NG"} ${label} ${detail}`);
		if (!ok) {
			failures.push(label);
		}
	};
	const open = async (rel) => vscode.workspace.openTextDocument(path.join(root, rel));
	const names = (items) => items.map((i) => i.name).sort();
	const doc = await vscode.workspace.openTextDocument(uri);
	const newBase = find(doc, "NewBase(", 1);
	const api = vscode.extensions.getExtension("toga4.golspk").exports;

	// Navigation.
	check("definition", (await definitions(uri, newBase)).join() === "a/a.go:16");
	check("hover shows the signature and the doc comment", /func a\.NewBase\(name string\) a\.Base[\s\S]*NewBase returns a Base/.test(await hoverText(uri, newBase)));
	const refs = await run("vscode.executeReferenceProvider", uri, newBase);
	check("references", refs.length === 2, `${refs.length}`);
	const symbols = await run("vscode.executeWorkspaceSymbolProvider", "NewBase");
	check("workspace symbol", names(symbols).includes("NewBase"));

	// Hierarchies and implementations, including embedding, generics and test files.
	const calls = await run("vscode.prepareCallHierarchy", uri, newBase);
	const incoming = await run("vscode.provideIncomingCalls", calls[0]);
	check("incoming calls", incoming.map((c) => c.from.name).join() === "Run");
	const run_ = await run("vscode.prepareCallHierarchy", uri, find(doc, "func Run", 6));
	const outgoing = await run("vscode.provideOutgoingCalls", run_[0]);
	check("outgoing calls", names(outgoing.map((c) => c.to)).join() === "Base.RunSomethingLong,NewBase", names(outgoing.map((c) => c.to)).join());
	const aDoc = await open("a/a.go");
	const runner = find(aDoc, "Runner interface", 1);
	const subtypes = async () => names(await run("vscode.provideSubtypes", (await run("vscode.prepareTypeHierarchy", aDoc.uri, runner))[0]));
	check("subtypes find embedding, generic and test implementations", (await subtypes()).join() === "Base,Box,Embedder,ViaInterface,mockRunner", (await subtypes()).join());
	const impls = await run("vscode.executeImplementationProvider", aDoc.uri, runner);
	check("implementations", impls.length === 5, `${impls.length}`);
	const cDoc = await open("c/c.go");
	const box = find(cDoc, "Box[T any]", 1);
	const supertypes = names(await run("vscode.provideSupertypes", (await run("vscode.prepareTypeHierarchy", cDoc.uri, box))[0]));
	check("supertypes of a generic type", supertypes.join() === "Getter,Runner", supertypes.join());

	// Search settings.
	check("tests button is shown", /Tests: included|テスト: 含む/.test(api.ui().testsButton), api.ui().testsButton);
	await run("golspk.toggleExcludeTests");
	check("excluding tests removes the test implementation", (await subtypes()).join() === "Base,Box,Embedder,ViaInterface");
	await run("golspk.toggleExcludeTests");
	await vscode.workspace.getConfiguration("golspk").update("exclude", ["b"], vscode.ConfigurationTarget.Global);
	const globbed = await until(subtypes, (s) => s.join() === "Base,Box,mockRunner");
	check("an exclude glob removes the files under a directory", globbed.ok, globbed.value.join());
	await vscode.workspace.getConfiguration("golspk").update("exclude", undefined, vscode.ConfigurationTarget.Global);
	await until(subtypes, (s) => s.length === 5);

	// Widening the search for a symbol of the standard library to all modules of the workspace.
	const errorsNew = find(doc, "errors.New(", 8);
	const modules = (locs) => [...new Set(locs.map((l) => (vscode.workspace.asRelativePath(l.uri).startsWith("other/") ? "other" : "implmod")))].sort().join();
	const limited = await run("vscode.executeReferenceProvider", uri, errorsNew);
	check("references to a standard library symbol stay in the module", modules(limited) === "implmod", modules(limited));
	check("the server reports the narrowed search", api.lastLimited()?.name === "New", JSON.stringify(api.lastLimited()));
	check("widen button is shown", /errors\.New|New/.test(api.ui().widenButton), api.ui().widenButton);
	const widened = await run("golspk.widenLastSearch");
	check("widening searches all modules", modules(widened) === "implmod,other", modules(widened));

	// Following changes made to files on disk by another process.
	const aFile = path.join(root, "a/a.go");
	const dFile = path.join(root, "d/d.go");
	const addedFile = path.join(root, "a/added.go");
	const original = fs.readFileSync(aFile, "utf8");
	fs.writeFileSync(aFile, original.replace("func NewBase(name string)", "func NewBase(renamed string)"));
	let r = await until(() => hoverText(uri, newBase), (text) => text.includes("renamed string"));
	check("a changed dependency shows up in hover", r.ok, `${r.seconds}s`);
	fs.writeFileSync(aFile, fs.readFileSync(aFile, "utf8").replace("package a\n", "package a\n\n// line 1\n// line 2\n"));
	r = await until(() => definitions(uri, newBase), (d) => d.join() === "a/a.go:19");
	check("added lines shift go to definition", r.ok, `${r.value.join()} ${r.seconds}s`);
	fs.writeFileSync(addedFile, "package a\n\n// Added is added while the server is running.\nfunc Added() string { return \"\" }\n");
	const dSource = fs.readFileSync(dFile, "utf8");
	fs.writeFileSync(dFile, `${dSource}\nfunc callAdded() string { return a.Added() }\n`);
	const added = () => find(doc, "a.Added()", 3);
	r = await until(async () => (doc.getText().includes("a.Added()") ? definitions(uri, added()) : []), (d) => d.join() === "a/added.go:4");
	check("an added file resolves from an open file", r.ok, `${r.value.join()} ${r.seconds}s`);
	const symbolNames = async (query) => names(await run("vscode.executeWorkspaceSymbolProvider", query));
	r = await until(() => symbolNames("Added"), (n) => n.includes("Added"));
	check("an added file shows up in workspace symbols", r.ok, `${r.seconds}s`);
	fs.unlinkSync(addedFile);
	fs.writeFileSync(dFile, dSource);
	fs.writeFileSync(aFile, original);
	r = await until(() => symbolNames("Added"), (n) => !n.includes("Added"));
	check("a removed file disappears from workspace symbols", r.ok, `${r.seconds}s`);
	r = await until(() => hoverText(uri, newBase), (text) => text.includes("name string"));
	check("a reverted dependency shows up in hover", r.ok, `${r.seconds}s`);

	if (failures.length > 0) {
		throw new Error(`failed: ${failures.join("; ")}`);
	}
}

exports.run = async () => {
	const uri = vscode.Uri.file(process.env.GOLSPK_TEST_FILE);
	const doc = await vscode.workspace.openTextDocument(uri);
	await vscode.window.showTextDocument(doc);
	log(`language: ${vscode.env.language}`);
	if (process.env.GOLSPK_TEST_FIXTURE) {
		await measure(uri, find(doc, "NewBase(", 1));
		await checkFixture(uri);
		return;
	}
	await measure(uri, new vscode.Position(Number(process.env.GOLSPK_TEST_LINE) - 1, Number(process.env.GOLSPK_TEST_COL) - 1));
};
