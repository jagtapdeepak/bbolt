# Page checksums (Approach A) — staged plan

See [ANALYSIS.md](ANALYSIS.md) for why we chose per-page CRC-64 on Get (not Merkle, not Check-only).

**Goal:** version-3 files store a CRC on every page; `Get` / cursor verify it; `GetWithError` surfaces bit rot. Version-2 files still open without page CRCs.

**Out of scope:** Merkle parent/child hashes, cryptographic MACs, in-place v2 conversion, changing mmap or two-phase commit.

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

Make new databases write checksummed pages. Do **not** verify on read yet (existing tests should keep passing once `PageHeaderSize` and `init` are consistent).

| Item | Detail |
|------|--------|
| Version | Bump `common.Version` to **3**. `Meta.Validate` accepts **2 and 3**. `Open` / `init` write 3. |
| Header | Add `checksum uint64` after `overflow` on `Page` ([`internal/common/page.go`](internal/common/page.go)). Header 16→24 bytes. Do **not** change `branchPageElement`, `Inode`, or `Meta`. |
| CRC | CRC-64 ISO (`hash/crc64`) over `(overflow+1)*pageSize` with the checksum field treated as **zero**. Helpers: `ChecksumPage(p, pageSize)`, `SetPageChecksum`. |
| Write | After `node.write`, `freelist.Write`, and [`DB.init`](db.go) empty pages: set the CRC before `writeAt`. Same for meta pages (page CRC in addition to existing FNV). |
| Errors | Add `errors.ErrPageChecksum` now; unused until Stage 2. |

**Done when:** a new DB’s leaf/branch/freelist/meta pages have a matching CRC if inspected by a unit test that hashes the raw page. `go test` (no corruption cases yet) is green.

**Files:** `internal/common/page.go`, `types.go`, `errors/errors.go`, `node.go`, `db.go` (`init`), freelist `Write` callers / `tx.commitFreelist`.

---

## Stage 2 — Verify on read and public API

Detect rot on the Get/cursor path.

| Item | Detail |
|------|--------|
| Hook | After `FastCheck` in [`Tx.page`](tx.go): if `meta.version >= 3`, recompute CRC; mismatch sets `tx.err` and returns a typed error. **Do not panic.** Version 2: skip. |
| Cache | Per-Tx `map[Pgid]struct{}` of verified pages so `seek` does not rehash the root on every Get. |
| `Get` | Signature unchanged. On rot: return `nil` and set `tx.err` (same as a miss unless the caller checks `Tx.Err()`). |
| `GetWithError` | `([]byte, error)`: miss → `nil, nil`; rot → `nil, ErrPageChecksum` (include pgid). |
| Cursor | Keep `First` / `Next` / `Seek` signatures; add `Cursor.Err()`. |
| Managed txs | [`View`](db.go) / [`Update`](db.go): if the closure returns nil but `tx.err != nil`, return `tx.err`. Add `Tx.Err()`. |

**Done when:** flipping one byte in a committed leaf makes `GetWithError` fail; a normal `Get` of a good key still works; v2 behavior unchanged.

**Files:** `tx.go`, `bucket.go`, `cursor.go`, `db.go`.

---

## Stage 3 — Full-tree Check, compact, CLI

Cover unread pages and migration.

| Item | Detail |
|------|--------|
| `Tx.Check` | For v3, CRC every reachable page (and overflow span). Report pgid. `StrictMode` after commit includes this. |
| Compact | [`compact`](compact.go) / `bbolt compact` writes a **v3** file (recompute CRCs). v2 source still readable. |
| CLI | `bbolt check` surfaces CRC errors. `bbolt page` / inspect prints `Checksum`. |

**Done when:** `Check` finds rot on a page never `Get`’d; compact of a v2 fixture produces a v3 file that verifies.

**Files:** `tx_check.go`, `compact.go`, `cmd/bbolt/command/command_check.go`, page inspect commands.

---

## Then: tests

Run after Stage 3 (add tests incrementally per stage so each stage stays green).

**Stage 1 tests**

- CRC of a synthetic page; checksum field excluded from the hash; overflow span.
- `DB.init` / first commit: every page type has a valid CRC.

**Stage 2 tests**

- Flip a byte in a leaf: `GetWithError` → `ErrPageChecksum`; `Get` → nil and `Tx.Err()` set.
- Flip a byte in a branch (including a child pgid): lookup that walks that page fails.
- Missing key still `nil, nil` from `GetWithError`.
- `View` returns `ErrPageChecksum` if the closure used `Get` on a rotten page and returned nil.
- Open a v2 file: no page CRC checks; reads succeed.

**Stage 3 tests**

- Rotten unread page: `Get` of another key succeeds; `Check` reports the rotten pgid.
- Compact v2 → v3; then corruption on the v3 file is detected.
- Document (do not test as a failure): a **swapped valid page** is not detected (Approach A gap in ANALYSIS.md).

**Commands:** `go test ./...` and targeted packages (`go test .`, `go test ./internal/common`, `go test ./cmd/bbolt/command`).

---

## Then: performance analysis

Compare **this branch vs main** (or vs a build with verification stubbed off) after tests pass. Record numbers in this file or a short subsection of ANALYSIS.md.

**Microbenchmarks** (in-process, warm mmap)

| Bench | What it measures |
|-------|------------------|
| `BenchmarkGet` | Point `Get` of an existing key, height 2–4 |
| `BenchmarkGetWithError` | Same with the new API |
| `BenchmarkCursorNext` | Sequential scan of N keys (Tx cache should keep this close to baseline) |
| `BenchmarkCursorSeek` | Repeated `Seek` (cache hit on internal nodes) |
| `BenchmarkUpdatePut` | Single `Put` + `Commit` (CRC vs two `fdatasync`s) |
| `BenchmarkCheck` | `Tx.Check` on a DB of known size (e.g. 100MiB / 1GiB if practical) |

Put benches in `db_test.go` or `checksum_bench_test.go`. Run:

```bash
go test -bench='BenchmarkGet|BenchmarkCursor|BenchmarkUpdatePut|BenchmarkCheck' -benchmem -count=5 .
```

**What to report**

- ns/op and Δ% vs baseline for Get, Cursor.Next, Put+Commit, Check.
- Confirm Cursor.Next regression stays small **with** the per-Tx cache (if it does not, the cache is broken).
- Optional: `cmd/bbolt bench` for a coarser end-to-end number.

**Expectations** (from ANALYSIS.md; validate, do not assume)

- Warm Get: a few µs extra (CRC × height); visible in microbenches.
- Cursor.Next with cache: near baseline.
- Commit: noise vs `fdatasync`.
- Check: extra CPU ≈ size / CRC throughput, often still I/O-bound.

If Get regression is large, profile `Tx.page` / CRC and consider CRC-32C later — that is a follow-up, not Stage 1–3.

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
