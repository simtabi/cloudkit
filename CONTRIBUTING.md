# Contributing

Thanks for helping. cloudkit is a library that other products trust with provider credentials and
with their network egress, so its rules are strict.

## Workflow

1. Branch from current `main` as `<type>/<slug>` (`feat`, `fix`, `chore`, `ci`, `docs`).
2. Run the gates locally: `make ci` (gofmt check, vet, tests with the race detector, policy tests)
   and `make lint`.
3. Add an `[Unreleased]` entry to `CHANGELOG.md`.
4. Open a pull request into `main`. Nothing reaches `main` any other way.

The Go module is `src/`. Run Go as `go -C src …` or through the Makefile; a bare `go test ./...`
at the repository root finds no packages and passes.

## Rules for code

| Rule | Detail |
|---|---|
| Network | Every request goes through the injected `Transport`. Never `http.DefaultClient`, `http.DefaultTransport`, `http.Get` or `net.Dial` |
| TLS and SSH | No `InsecureSkipVerify`, no `InsecureIgnoreHostKey`, no SSH password auth |
| State | No `init()` registration and no package-level clients |
| Logging | Use the injected `*slog.Logger`; never the default logger |
| Credentials | Carry them as `cloudkit.Secret`; pass errors that may hold a URL or body through `Redact` |
| Test credentials | Generate them at run time (`crypto/rand.Text()`); never a literal |
| Dependencies | Range-friendly module versions; licences MIT, BSD, Apache-2.0, ISC or MPL-2.0 only |
| Guards | A new rule lands with a test, and the test is seen failing before it passes |

The policy test in `src/internal/policy` enforces the first four rows. Its exemption list carries a
reason and an exact count per entry; raise a count only with a reason a reviewer accepts.

## Commits

Imperative subject of 72 characters or fewer; the body says why.

---

Security reports go through [SECURITY.md](SECURITY.md), never a public issue.
