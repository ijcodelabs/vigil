#!/bin/sh
# Prints the CHANGELOG.md section for a release tag, which becomes the text
# of its GitHub release:
#
#	scripts/release-notes.sh v0.2.0 [CHANGELOG.md]
#
# A section starts at "## v0.2.0" (anything after a space, such as the date,
# is ignored) and ends at the next "## ". A release candidate (v0.2.0-rc.1)
# uses its version's section once it is there, otherwise "## Unreleased", so
# a candidate can be cut before the changelog is renamed. A release without
# a section of its own fails: a tag should not be published without notes.
set -eu

tag=${1:?usage: release-notes.sh vX.Y.Z[-rc.N] [CHANGELOG.md]}
file=${2:-CHANGELOG.md}

if ! printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$'; then
	echo "not a release tag: $tag (want vX.Y.Z or vX.Y.Z-rc.N)" >&2
	exit 1
fi
base=${tag%%-*}

section() {
	awk -v h="## $1" '
		$0 == h || index($0, h " ") == 1 { on = 1; next }
		/^## / { if (on) exit }
		on
	' "$file"
}

notes=$(section "$base")
if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ] && [ "$base" != "$tag" ]; then
	notes=$(section Unreleased)
fi
if [ -z "$(printf '%s' "$notes" | tr -d '[:space:]')" ]; then
	echo "$file has no section for $base" >&2
	exit 1
fi
printf '%s\n' "$notes"
