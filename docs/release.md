# Release

How a cloudkit version is tagged and released. cloudkit is a library, so a release is two git tags
and a GitHub release whose body is the version's CHANGELOG section. There are no binaries.

## Steps

1. Move the `[Unreleased]` entries in `CHANGELOG.md` under a dated `## [X.Y.Z] - YYYY-MM-DD`
   heading, through a pull request.
2. After it merges, tag the merge commit and push the tag. Pushing the tag is the release:

   ```bash
   git -C ~/Artisan/projects/opensource/simtabi/packages/cloudkit tag vX.Y.Z origin/main
   git -C ~/Artisan/projects/opensource/simtabi/packages/cloudkit push origin refs/tags/vX.Y.Z
   ```

   The release workflow starts on the pushed tag. It checks that the tag is a `vX.Y.Z` (or
   `vX.Y.Z-rc.N`) tag on GitHub, checks it out, creates `src/vX.Y.Z` on the same commit (Go resolves
   a module in a subdirectory only through that tag), then creates the GitHub release with the
   CHANGELOG section as its body. A tag whose version has no CHANGELOG section stops the release.
3. To release a tag that is already on GitHub (a first release of an older tag, or a run that
   failed), ask for it by name:

   ```bash
   gh workflow run release.yml --repo simtabi/cloudkit -f tag=vX.Y.Z
   ```

   It does the same as the tag push. `vX.Y.Z` must be a real tag already on GitHub; any other value
   is refused before anything is published. Re-running it on a released tag updates that release's
   notes and leaves both tags alone.

cloudkit is the library exception to the org's shared Go release workflow
(`simtabi/.github`'s `go-release.yml`): that workflow builds binaries with GoReleaser, and cloudkit
has none. It keeps its own release job, on the same two triggers.

## Rules

| Rule | Why |
|---|---|
| Tags are final: never moved, deleted or reused | The Go module proxy caches a tag the first time anything fetches it, and that cache cannot be cleared |
| Push a `vX.Y.Z` tag only for final code | A pushed version tag releases |
| Testing uses `vX.Y.Z-rc.N` | A pre-release tag burns no release number |
| A bad version is retracted with `retract vX.Y.Z` in `go.mod`, then the next version ships | The proxy keeps serving the bad one |
| `release.yml` runs on a pushed version tag or on request, and on nothing else | A test in `src/internal/policy` fails if it gains a branch push, `schedule`, `pull_request` or `release` trigger, a tag filter that is not a version, or stops releasing from the tag it names |

---

[← Docs index](../README.md#documentation)
