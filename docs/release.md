# Release

How a cloudkit version is tagged and released. cloudkit is a library, so a release is two git tags
and a GitHub release whose body is the version's CHANGELOG section. There are no binaries.

## Steps

0. Check the workflow is registered: `gh workflow list --repo simtabi/cloudkit` lists `Release`.
   GitHub registers a dispatch-only workflow only when a push to `main` changes it, so a fresh copy
   of the repository answers 404 until then.
1. Move the `[Unreleased]` entries in `CHANGELOG.md` under a dated `## [X.Y.Z] - YYYY-MM-DD`
   heading, through a pull request.
2. After it merges, tag the merge commit and push the tag:

   ```bash
   git -C ~/Artisan/projects/opensource/simtabi/packages/cloudkit tag vX.Y.Z origin/main
   git -C ~/Artisan/projects/opensource/simtabi/packages/cloudkit push origin refs/tags/vX.Y.Z
   ```

   Pushing the tag publishes nothing.
3. Dispatch the release workflow on that tag, with the confirmation:

   ```bash
   gh workflow run release.yml --repo simtabi/cloudkit --ref vX.Y.Z -f confirm=release-go
   ```

   `vX.Y.Z` must be a real tag already on GitHub. The workflow refuses to run without
   `confirm=release-go` or on anything but a `v` tag. It creates `src/vX.Y.Z` on the same commit
   (Go resolves a module in a subdirectory only through that tag), then creates the GitHub release
   with the CHANGELOG section as its body. It runs only for a version the owner approved.

## Rules

| Rule | Why |
|---|---|
| Tags are final: never moved, deleted or reused | The Go module proxy caches a tag the first time anything fetches it, and that cache cannot be cleared |
| Testing uses `vX.Y.Z-rc.N` | A pre-release tag burns no release number |
| A bad version is retracted with `retract vX.Y.Z` in `go.mod`, then the next version ships | The proxy keeps serving the bad one |
| `release.yml` has `workflow_dispatch` as its only trigger | A test in `src/internal/policy` fails if it gains a `push`, `schedule`, `pull_request` or `release` trigger, or loses the confirmation |

---

[← Docs index](../README.md#documentation)
