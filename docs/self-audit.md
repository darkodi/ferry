# Self-audit

Security issues found in Ferry's own code, recorded before or with the fix.

| ID | Severity | Component | Description | Status | Fix PR |
| --- | --- | --- | --- | --- | --- |
| SA-001 | Low | Dependencies | `govulncheck` flagged GO-2026-6865 (panic on crafted zstd frames) in `github.com/klauspost/compress` v1.20.1, pulled in by `testcontainers-go` for Docker archive handling. Ferry never decompresses untrusted zstd data, so the reachable path is test tooling, but the scan gate fails and the fix is a patch release. Bumped to v1.20.2. | Fixed | M3 `m3/outbox-relay` |
