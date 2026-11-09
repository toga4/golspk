// Checks that every localizable string has a Japanese translation and that no translation is left unused.
// VS Code only switches the display language when a language pack is installed, which the test instance does not have by default.
// This check finds missing translations without one.
const fs = require("node:fs");
const path = require("node:path");

const root = path.resolve(__dirname, "..");
const read = (file) => fs.readFileSync(path.join(root, file), "utf8");
const failures = [];
const compare = (label, want, got) => {
	for (const key of want) {
		if (!got.has(key)) {
			failures.push(`${label}: missing ${JSON.stringify(key)}`);
		}
	}
	for (const key of got) {
		if (!want.has(key)) {
			failures.push(`${label}: unused ${JSON.stringify(key)}`);
		}
	}
};

// Strings passed to vscode.l10n.t in extension.js against the Japanese bundle.
const used = new Set([...read("extension.js").matchAll(/\bt\(\s*"((?:[^"\\]|\\.)*)"/g)].map((m) => JSON.parse(`"${m[1]}"`)));
compare("l10n/bundle.l10n.ja.json", used, new Set(Object.keys(JSON.parse(read("l10n/bundle.l10n.ja.json")))));

// %key% placeholders in package.json against both package.nls files.
const placeholders = new Set([...read("package.json").matchAll(/"%([^%"]+)%"/g)].map((m) => m[1]));
for (const file of ["package.nls.json", "package.nls.ja.json"]) {
	compare(file, placeholders, new Set(Object.keys(JSON.parse(read(file)))));
}

if (failures.length > 0) {
	console.error(failures.join("\n"));
	process.exit(1);
}
console.log(`l10n check: ${used.size} runtime strings and ${placeholders.size} manifest strings are translated`);
