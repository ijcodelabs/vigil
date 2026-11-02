#!/bin/sh
# Every GitHub Action is pinned to a commit and every container image to a
# digest: a tag can be moved to other code, a commit or a digest cannot.
# Dependabot keeps the pins current (.github/dependabot.yml).
set -eu
bad=0
for f in .github/workflows/*.yml; do
	if grep -nE '^\s*-?\s*uses:' "$f" | grep -vE 'uses: [^@ ]+@[0-9a-f]{40}( |$)' | grep -vE 'uses: \./'; then
		echo "$f: an action not pinned to a commit (uses: owner/repo@<40-hex sha> # vX.Y.Z)"
		bad=1
	fi
done
for f in Dockerfile deploy/docker-compose.yml docs/install-by-ai.md Makefile .github/workflows/*.yml; do
	# FROM lines, image: lines and `docker run` images from a registry
	if grep -nE '^FROM |^\s*image: |docker run .*(ghcr\.io|docker\.io)/|(CONTAINER\)|podman) run .*docker\.io/' "$f" |
		grep -vE 'FROM scratch|@sha256:[0-9a-f]{64}|\$image|ghcr\.io/ijcodelabs/vigil|image: vigil:local'; then
		echo "$f: an image not pinned to a digest (name:tag@sha256:...)"
		bad=1
	fi
done
[ "$bad" = 0 ] && echo "all actions and images are pinned"
exit "$bad"
