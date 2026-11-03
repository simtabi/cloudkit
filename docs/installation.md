# Installation

How to add cloudkit to a Go module.

## Requirements

| Requirement | Value |
|---|---|
| Go | 1.27 or later (the module declares `go 1.27.0` and `toolchain go1.27.3`) |
| Module path | `github.com/simtabi/cloudkit/src` |
| Dependencies | the standard library only, for now |

## Adding it

The Go module sits in the repository's `src/` directory, so the import path ends in `/src`:

```bash
go get github.com/simtabi/cloudkit/src@main
```

```go
import "github.com/simtabi/cloudkit/src"
```

The package name is `cloudkit`.

## Pinning before v0.1.0

No version is tagged yet. `go get …@main` records a pseudo-version for the commit it resolved, and
`go.mod` keeps that commit until you update it. Once `v0.1.0` exists, depend on it instead:

```bash
go get github.com/simtabi/cloudkit/src@v0.1.0
```

Each release carries two tags on one commit, `vX.Y.Z` and `src/vX.Y.Z`; Go resolves the module
through the second, so both must exist. See [Release](release.md).

---

[← Docs index](../README.md#documentation)
