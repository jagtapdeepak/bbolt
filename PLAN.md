# Page checksums (Approach A) — staged plan

See [ANALYSIS.md](ANALYSIS.md) for layout (CRC at end of span, not header, not sidecar) and why.

**Goal:** new (version 3) databases store a CRC-32 at the end of each **leaf/branch** page span; `Get` verifies it. Header size stays 16 bytes. Meta keeps FNV only. Freelist has no page CRC yet.

**Version 2:** still opens without page CRC. **Upgrade to v3 is handled later by scrub** (rewrite reachable branch/leaf pages + bump meta version). This change does not implement scrub.

**Out of scope here:** Merkle, sidecar, growing the page header, `GetWithError`, compact-as-upgrade, scrub, meta trailing CRC, freelist page CRC.

```mermaid
flowchart LR
  S1[Stage 1 format and write]
  S2[Stage 2 read and API]
  S3[Stage 3 Check compact CLI]
  T[Tests]
  P[Performance analysis]
  S1 --> S2 --> S3 --> T --> P
```

---

## Stage 1 — On-disk format and write path

Make new databases write checksummed spans. Do **not** verify on read yet.

| Item | Detail |
|------|--------|
| Version | Bump `common.Version` to **3**. `Meta.Validate` accepts **2 and 3**. `Open` / `init` write 3. |
| Header | **Unchanged.** Do not add fields to `Page`. `PageHeaderSize` remains `unsafe.Sizeof(Page{})` (16). |
| CRC | CRC-32 Castagnoli (`hash/crc32`) of `[0 : spanLen-4]` (header and payload; not the trailing CRC). Stored in the last 4 bytes of `(overflow+1)*pageSize`. Helpers hash a `[]byte`; they do not parse page type. |
| Packing | If payload would overlap the last 4 bytes, allocate extra overflow (or leave slack), then stamp CRC. |
| Write | After `node.write` / [`DB.init`](db.go) empty **leaf**: set the CRC before `writeAt`. **Do not** stamp meta (FNV already) or freelist. |
| Errors | Add `errors.ErrPageChecksum` now; unused until Stage 2. |

**Done when:** a new DB’s leaf/branch spans have a matching trailing CRC. Meta and freelist do not.

**Files:** `internal/common/checksum.go` (or similar), `types.go`, `errors/errors.go`, `node.go` / spill page count, `tx.go` (`write`, `writeMeta`), `db.go` (`init`).

---

## Stage 2 — Verify on read and public API

Detect rot on the Get/cursor path.

| Item | Detail |
|------|--------|
| Hook | After `FastCheck` in [`Tx.page`](tx.go): if v3 and the page is leaf/branch, recompute CRC over header + payload. Skip meta and freelist. |
| Cache | Per-Tx `map[Pgid]struct{}` of verified pages so `seek` does not rehash the root on every Get. |
| `Get` | Signature unchanged. On rot: return `nil` and set `tx.err`. |
| `GetWithError` | `([]byte, error)`: miss → `nil, nil`; rot → `nil, ErrPageChecksum`. |
| Cursor | Keep `First` / `Next` / `Seek` signatures; add `Cursor.Err()`. |
| Managed txs | [`View`](db.go) / [`Update`](db.go): if the closure returns nil but `tx.err != nil`, return `tx.err`. Add `Tx.Err()`. |

**Done when:** flipping one byte in a committed leaf payload makes `GetWithError` fail; a normal `Get` of a good key still works; v2 behavior unchanged.

**Files:** `tx.go`, `bucket.go`, `cursor.go`, `db.go`.

---

## Stage 3 — Full-tree Check, compact, CLI

Cover unread pages and migration.

| Item | Detail |
|------|--------|
| `Tx.Check` | For v3, CRC every reachable span. Report pgid. `StrictMode` after commit includes this. |
| Compact | [`compact`](compact.go) / `bbolt compact` writes a **v3** file (recompute CRCs). v2 source still readable. This is the v2→v3 upgrade (no in-place header rewrite). |
| CLI | `bbolt check` surfaces CRC errors. |

**Done when:** `Check` finds rot on a page never `Get`’d; compact of a v2 fixture produces a v3 file that verifies.

**Files:** `tx_check.go`, `compact.go`, `cmd/bbolt/command/command_check.go`.

---

## Then: tests

**Stage 1 tests**

- CRC of a synthetic span; last 4 bytes excluded from the hash; header and payload included; overflow span.
- Packed page that needs extra overflow so the CRC does not overlap payload.
- `DB.init` / first commit: every page type has a valid trailing CRC.

**Stage 2 tests**

- Flip a byte in a leaf payload: `GetWithError` → `ErrPageChecksum`; `Get` → nil and `Tx.Err()` set.
- Flip a byte in a branch payload: lookup that walks that page fails.
- Missing key still `nil, nil` from `GetWithError`.
- `View` returns `ErrPageChecksum` if the closure used `Get` on a rotten page and returned nil.
- Open a v2 file: no page CRC checks; reads succeed.

**Stage 3 tests**

- Two keys in different leaves; corrupt L1. `Get(keyOnL2)` succeeds; `Get(keyOnL1)` → `ErrPageChecksum`; `Check` reports L1 even if it was never `Get`’d.
- Compact v2 → v3; then flip a byte on the dest: `Get` / `Check` fail as in Stage 2.
- Copy a CRC-valid leaf over another (do **not** treat as failure): `Get` / `Check` stay silent. Self-CRC does not bind a page to its pgid; Approach B would.

**Commands:** `go test ./...` and targeted packages (`go test .`, `go test ./internal/common`, `go test ./cmd/bbolt/command`).

## Test results

Image: `ghcr.io/jagtapdeepak/linux-build-env:latest` (2026-08-14).

```bash
go test ./internal/common/ -count=1
go test . -count=1 -timeout 20m -run 'TestChecksum|TestPagesForChecksum|TestPageChecksum|TestOpen|TestDB_Get|TestDB_Put|TestTx_Check|TestBucket_Stats'
```

| Result | Tests |
|--------|--------|
| Pass | `./internal/common`; `TestPageChecksum_*`; `TestTx_Check_*`; `TestBucket_Stats*` (RandomFill counts updated for the 4-byte CRC trailer) |
| Notes | `View` after `Check` on a rotten leaf returns `ErrPageChecksum` (`Tx.page` sets `tx.err`). |

---

## Then: performance analysis

Compare **this branch vs main** after tests pass.

**Microbenchmarks** (in-process, warm mmap)

| Bench | What it measures |
|-------|------------------|
| `BenchmarkGet` | Point `Get` of an existing key, height 2–4 |
| `BenchmarkGetWithError` | Same with the new API |
| `BenchmarkCursorNext` | Sequential scan of N keys (Tx cache should keep this close to baseline) |
| `BenchmarkCursorSeek` | Repeated `Seek` (cache hit on internal nodes) |
| `BenchmarkUpdatePut` | Single `Put` + `Commit` (CRC vs two `fdatasync`s) |
| `BenchmarkCheck` | `Tx.Check` on a DB of known size |

```bash
go test -bench='BenchmarkGet|BenchmarkCursor|BenchmarkUpdatePut|BenchmarkCheck' -benchmem -count=5 .
```

**Expectations** (from ANALYSIS.md; validate, do not assume)

- Warm Get: extra CRC × height; visible in microbenches.
- Cursor.Next with cache: near baseline.
- Commit: noise vs `fdatasync` (no sidecar write).
- Check: extra CPU ≈ size / CRC throughput, often still I/O-bound.

---

## Suggested order of work

1. Stage 1 + its unit tests  
2. Stage 2 + corruption tests on Get  
3. Stage 3 + Check/compact tests  
4. Full `go test ./...`  
5. Benchmarks vs baseline → fill in results below  

## Performance results

*(Fill in after the benchmark pass.)*

| Benchmark | Baseline ns/op | With CRC ns/op | Δ% |
|-----------|----------------|----------------|-----|
| Get | | | |
| Cursor.Next | | | |
| Put+Commit | | | |
| Check | | | |
