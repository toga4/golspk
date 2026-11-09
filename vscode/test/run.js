// Launches VS Code with a separate user data directory, loads the extension and runs test/suite.js.
//
// Usage:
//   node test/run.js --fixture
//     Checks every feature against a temporary copy of testdata/implmod. The suite rewrites files in the copy.
//   node test/run.js WORKSPACE_DIR FILE LINE COL
//     Measures the time until go to definition and hover respond at the 1-based position.
//
// Environment variables:
//   VSCODE_EXECUTABLE           The VS Code executable to launch. Defaults to the installed one on macOS, or a downloaded one.
//   GOLSPK_TEST_LOCALE          The display language to launch VS Code with, such as "ja".
//   GOLSPK_TEST_WITH_EXTENSIONS Also load the other extensions placed in .vscode-test/extensions, such as golang.go.
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
const { runTests } = require("@vscode/test-electron");

const INSTALLED_MACOS = "/Applications/Visual Studio Code.app/Contents/MacOS/Code";

async function main() {
	let [workspace, file, line, col] = process.argv.slice(2);
	const fixture = workspace === "--fixture";
	if (fixture) {
		// Resolve symlinks, because the temporary directory on macOS is one and the server reports resolved paths.
		workspace = fs.realpathSync(fs.mkdtempSync(path.join(os.tmpdir(), "golspk-fixture-")));
		fs.cpSync(path.resolve(__dirname, "../../testdata/implmod"), workspace, { recursive: true });
		file = path.join(workspace, "d/d.go");
	} else if (!col) {
		console.error("usage: node test/run.js --fixture | WORKSPACE_DIR FILE LINE COL");
		process.exit(2);
	}
	const launchArgs = [workspace];
	if (!process.env.GOLSPK_TEST_WITH_EXTENSIONS) {
		launchArgs.push("--disable-extensions");
	}
	if (process.env.GOLSPK_TEST_LOCALE) {
		launchArgs.push("--locale", process.env.GOLSPK_TEST_LOCALE);
	}
	const executable = process.env.VSCODE_EXECUTABLE || (fs.existsSync(INSTALLED_MACOS) ? INSTALLED_MACOS : undefined);
	try {
		await runTests({
			// Without an executable, @vscode/test-electron downloads VS Code.
			...(executable ? { vscodeExecutablePath: executable } : {}),
			extensionDevelopmentPath: path.resolve(__dirname, ".."),
			extensionTestsPath: path.resolve(__dirname, "suite.js"),
			launchArgs,
			extensionTestsEnv: {
				GOLSPK_TEST_FIXTURE: fixture ? "1" : "",
				GOLSPK_TEST_FILE: file,
				GOLSPK_TEST_LINE: line ?? "",
				GOLSPK_TEST_COL: col ?? "",
			},
		});
	} finally {
		if (fixture) {
			fs.rmSync(workspace, { recursive: true, force: true });
		}
	}
}

main().catch((err) => {
	console.error(err);
	process.exit(1);
});
