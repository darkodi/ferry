# Self-audit

Security issues found in Ferry's own code, recorded before or with the fix.

| ID | Severity | Component | Description | Status | Fix PR |
| --- | --- | --- | --- | --- | --- |
| SA-001 | Low | Dependencies | `govulncheck` flagged GO-2026-6865 (panic on crafted zstd frames) in `github.com/klauspost/compress` v1.20.1, pulled in by `testcontainers-go` for Docker archive handling. Ferry never decompresses untrusted zstd data, so the reachable path is test tooling, but the scan gate fails and the fix is a patch release. Bumped to v1.20.2. | Fixed | M3 `m3/outbox-relay` |
| SA-002 | Low | Dependencies | `govulncheck` flagged GO-2026-6278 (WebSocket mask key from a weak PRNG) in `github.com/gorilla/websocket` v1.5.0, required by `go-ethereum`'s RPC client. Ferry dials the EVM node over HTTP, so the WebSocket path is reachable only with a `ws://` RPC URL, but the fix is a patch release. Bumped to v1.5.3. | Fixed | M4 `m4/evm-deposits` |
| SA-003 | Low | Chain watcher | `Watcher.confirm` compared a deposit's stored block hash with the chain's using `strings.EqualFold`. That is harmless for EVM hex hashes, but Solana blockhashes are base58, where case is significant: two different hashes differing only in letter case would compare equal, so a replaced block could pass the re-check before crediting. Producing such a pair is not practical, but the comparison was wrong for the data. Now an exact comparison. | Fixed | M4 `m4/solana-deposits` |
