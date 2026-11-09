const path = require("node:path");
const vscode = require("vscode");
const { LanguageClient, TransportKind } = require("vscode-languageclient/node");

const t = vscode.l10n.t;

// How long the button that widens a search stays visible after the server reports a narrowed search, in milliseconds.
const LIMITED_BUTTON_MS = 60000;

const EXCLUDE_TESTS_KEY = "golspk.excludeTests";

/** @type {LanguageClient | undefined} */
let client;

/**
 * The last search that the server narrowed to the module of the open file.
 * @type {{ method: "references" | "implementation", uri: string, position: { line: number, character: number }, name: string } | undefined}
 */
let lastLimited;

/** @type {vscode.StatusBarItem} */
let widenButton;
/** @type {NodeJS.Timeout | undefined} */
let widenTimer;
/** @type {vscode.StatusBarItem} */
let testsButton;
/** @type {vscode.ExtensionContext} */
let extensionContext;

/** Returns the settings to pass to the server. Whether tests are excluded is remembered per workspace. */
function currentOptions() {
	const config = vscode.workspace.getConfiguration("golspk");
	return {
		exclude: config.get("exclude") ?? [],
		excludeTests: extensionContext.workspaceState.get(EXCLUDE_TESTS_KEY, false),
		trimAfterMinutes: config.get("trimAfterMinutes"),
		unloadAfterMinutes: config.get("unloadAfterMinutes"),
	};
}

function showTestsButton() {
	const excluded = extensionContext.workspaceState.get(EXCLUDE_TESTS_KEY, false);
	testsButton.text = excluded ? t("$(beaker) Tests: excluded") : t("$(beaker) Tests: included");
	testsButton.tooltip = t("Toggle whether golspk searches (references, implementations, call and type hierarchies, symbols) include _test.go files.");
	testsButton.show();
}

async function sendOptions() {
	if (client) {
		await client.sendNotification("golspk/setOptions", currentOptions());
	}
}

/** @param {vscode.ExtensionContext} context */
async function start(context) {
	const configured = vscode.workspace.getConfiguration("golspk").get("serverPath");
	const command = configured || path.join(context.extensionPath, "bin", "golspk");
	client = new LanguageClient(
		"golspk",
		"golspk",
		{ command, args: [], transport: TransportKind.stdio },
		{ documentSelector: [{ language: "go", scheme: "file" }], initializationOptions: currentOptions() },
	);
	client.onNotification("golspk/searchLimited", (params) => {
		lastLimited = params;
		widenButton.text =
			params.method === "references"
				? t("$(search) Find references to {0} in all modules", params.name)
				: t("$(search) Find implementations of {0} in all modules", params.name);
		widenButton.tooltip = t("golspk searched only the module of the open file. Click to search every module in the workspace.");
		widenButton.show();
		clearTimeout(widenTimer);
		widenTimer = setTimeout(() => widenButton.hide(), LIMITED_BUTTON_MS);
	});
	await client.start();
}

async function stop() {
	const c = client;
	client = undefined;
	if (c) {
		await c.stop();
	}
}

/**
 * Searches every module in the workspace for references or implementations and shows the results in the peek view.
 * @param {"references" | "implementation"} method
 * @param {vscode.Uri} uri
 * @param {vscode.Position} position
 * @returns {Promise<vscode.Location[]>}
 */
async function searchAllModules(method, uri, position) {
	if (!client) {
		return [];
	}
	const c = client;
	const title =
		method === "references"
			? t("golspk: Finding references in all modules")
			: t("golspk: Finding implementations in all modules");
	const locations = await vscode.window.withProgress({ location: vscode.ProgressLocation.Window, title }, async () => {
		const result = await c.sendRequest(`golspk/${method}`, {
			textDocument: { uri: c.code2ProtocolConverter.asUri(uri) },
			position: c.code2ProtocolConverter.asPosition(position),
			context: { includeDeclaration: true },
		});
		return (result ?? []).map((l) => c.protocol2CodeConverter.asLocation(l));
	});
	widenButton.hide();
	if (locations.length === 0) {
		const message = method === "references" ? t("golspk: No references found") : t("golspk: No implementations found");
		vscode.window.setStatusBarMessage(message, 5000);
		return locations;
	}
	await vscode.commands.executeCommand("editor.action.showReferences", uri, position, locations);
	return locations;
}

/** @param {"references" | "implementation"} method */
function searchAtCursor(method) {
	const editor = vscode.window.activeTextEditor;
	if (!editor) {
		return Promise.resolve([]);
	}
	return searchAllModules(method, editor.document.uri, editor.selection.active);
}

/** @param {vscode.ExtensionContext} context */
async function activate(context) {
	extensionContext = context;
	widenButton = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Left, 0);
	widenButton.command = "golspk.widenLastSearch";
	testsButton = vscode.window.createStatusBarItem(vscode.StatusBarAlignment.Right, 100);
	testsButton.command = "golspk.toggleExcludeTests";
	showTestsButton();
	context.subscriptions.push(
		widenButton,
		testsButton,
		vscode.commands.registerCommand("golspk.toggleExcludeTests", async () => {
			const next = !context.workspaceState.get(EXCLUDE_TESTS_KEY, false);
			await context.workspaceState.update(EXCLUDE_TESTS_KEY, next);
			showTestsButton();
			await sendOptions();
			return next;
		}),
		vscode.workspace.onDidChangeConfiguration((e) => {
			if (["golspk.exclude", "golspk.trimAfterMinutes", "golspk.unloadAfterMinutes"].some((key) => e.affectsConfiguration(key))) {
				sendOptions();
			}
		}),
		vscode.commands.registerCommand("golspk.restart", async () => {
			await stop();
			await start(context);
		}),
		vscode.commands.registerCommand("golspk.referencesAllModules", () => searchAtCursor("references")),
		vscode.commands.registerCommand("golspk.implementationsAllModules", () => searchAtCursor("implementation")),
		vscode.commands.registerCommand("golspk.widenLastSearch", () => {
			if (!lastLimited || !client) {
				return Promise.resolve([]);
			}
			const uri = client.protocol2CodeConverter.asUri(lastLimited.uri);
			const position = client.protocol2CodeConverter.asPosition(lastLimited.position);
			return searchAllModules(lastLimited.method, uri, position);
		}),
	);
	await start(context);
	// The tests use this to check the notifications received from the server and the text shown in the UI.
	return {
		lastLimited: () => lastLimited,
		ui: () => ({ testsButton: testsButton.text, widenButton: widenButton.text }),
	};
}

async function deactivate() {
	clearTimeout(widenTimer);
	await stop();
}

module.exports = { activate, deactivate };
