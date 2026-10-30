# Releasing

How a version of vigil is made. Set up 2026-10-30 and first used for
`v0.1.0-rc.1` the same day: archives, checksums, attestations and the
images all worked. The ghcr.io packages were public from their first push
(they belong to the account that owns the public repository and are
linked to it), so nothing has to be switched on there.

## The rules

- A release is a commit of `main` on GitHub, tagged `vX.Y.Z`. Nothing is
  assembled from chosen pieces: what is not ready stays switched off or
  unwired on `main`, as the Foundations already ask ("inert until
  enabled"). Cherry-picking features into a release was rejected, because
  that is where release processes break.
- Patch releases come from a `release/X.Y` branch, made from the `vX.Y.0`
  tag when the first fix is needed. A fix lands on `main` first and is
  cherry-picked; the branch only ever gets fixes.
- Release candidates are `vX.Y.Z-rc.N`, marked as pre-releases, for trying
  a release on the trial host before it is final.
- `CHANGELOG.md` is written by hand: every user-visible change adds its
  line under **Unreleased** in its own pull request, like README and docs.
  The release text is that section, so it is written for people who run
  vigil, not as a list of commits.
- The release is a **draft** until a person publishes it. The images are
  built only on publishing, so nothing public appears from a tag alone.

## Steps

1. Draft the changelog: the commits since the last tag
   (`git log --oneline v0.1.0..main`) against what is already under
   **Unreleased**; fill the gaps, and write the upgrade notes.
2. Look at what the release will hold, locally:
   `make release VERSION=v0.2.0` (archives and `SHA256SUMS` in `dist/`)
   and `make release-notes VERSION=v0.2.0`.
3. A pull request "Release v0.2.0" that renames `## Unreleased` to
   `## v0.2.0 (2026-11-14)` and adds an empty `## Unreleased` above it.
   Merge it.
4. Tag the merge commit on `main` and push the tag to GitHub:
   `git tag -a v0.2.0 -m v0.2.0 && git push github v0.2.0`.
5. The workflow (`.github/workflows/release.yml`) checks the tag's form,
   that it is on `main` or `release/*`, that the changelog has its section,
   and runs gofmt, vet, the tests, the size budgets and gitleaks. Then it
   builds the archives, signs `SHA256SUMS` keyless with cosign, attests
   every archive, and creates the draft release.
6. Read the draft, download one archive and verify it (the commands are in
   `docs/install.md`, "Download a release"), then **Publish**.
7. Publishing builds the multi-arch images (agent, server, relay), pushes
   them to `ghcr.io/ijcodelabs/vigil-*` with the version and minor tags,
   signs and attests them.

A failed workflow run is fixed on `main` and the tag is made again only if
the draft was never published: delete the draft and the tag
(`git push github :refs/tags/v0.2.0`), then tag again. A published version
is never replaced; the fix becomes the next patch release.

## Why it is built this way

- **A plain workflow and the Makefile, not GoReleaser.** The build, the
  size budgets and the committed eBPF objects are already defined in the
  Makefile; GoReleaser would be a second definition of the same build to
  keep in step. `make release` produces exactly what the workflow uploads,
  so a release can be looked at before it is tagged.
- **Keyless signing.** Sigstore signs with the workflow's own GitHub
  identity, so there is no signing key to keep, rotate or leak, and the
  repository's rule of no keys anywhere holds. The certificate names the
  workflow file and the tag, which is what a user verifies against. GitHub
  build attestations give the same for `gh attestation verify`.
- **One signature over `SHA256SUMS`**, not one per archive: verifying the
  checksums file and then the checksum is the usual chain, and keeps the
  release page short.
- **No `latest` image tag.** A deployment should name the version it runs;
  `0.2` follows patch releases, which is as far as following should go.
- **The images are cross-compiled** (`FROM --platform=$BUILDPLATFORM` in the
  Dockerfile), so the arm64 image needs no emulation.

## Not done yet

- On GitHub: a tag ruleset so that only the owner can create `v*` tags,
  and immutable releases, so a published version cannot be changed. Both
  are repository settings.
- The Kubernetes manifests in `deploy/` still name `vigil-agent:dev`; point
  them at the released images once the first one exists.
- Packages (`.deb`, `.rpm`) only if someone needs them; `nfpm` would add
  them without changing the rest.
