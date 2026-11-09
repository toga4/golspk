#!/bin/bash
# Prints the section of CHANGELOG.md for the version given as the argument, such as 0.1.0.
set -euo pipefail

version=${1:?usage: changelog-section.sh VERSION}
changelog="$(dirname "$0")/../CHANGELOG.md"
section=$(awk -v heading="## ${version}" '$0 == heading { found = 1; next } /^## / { found = 0 } found' "${changelog}" | sed -e '/./,$!d')
if [ -z "${section}" ]; then
	echo "CHANGELOG.md has no section for ${version}" >&2
	exit 1
fi
printf '%s\n' "${section}"
