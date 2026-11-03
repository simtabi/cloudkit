## Summary

<!-- What changes and why. -->

## Checks

- [ ] `make ci` and `make lint` pass locally
- [ ] New behaviour has tests; a new guard was seen failing before it passed
- [ ] No request bypasses the injected `Transport`, and no credential reaches a log, error or test literal
- [ ] `CHANGELOG.md` has an `[Unreleased]` entry
