# On-disk bit-rot detection

Today only the `Meta` struct (pages 0 and 1) has a checksum (FNV-1a). Branch and leaf pages (and their overflow) are unhashed. Freelist pages are also unhashed. `Get` returns mmap bytes with no integrity check. A flipped bit in a value is returned as good data.

This note records **where the page CRC lives**, why a **sidecar file** was rejected, then compares detect-on-Get vs Merkle vs Check-only. Staged implementation is in [PLAN.md](PLAN.md).

API: keep `Get(key []byte) []byte`. Add `GetWithError(key []byte) ([]byte, error)` so a missing key is `nil, nil` and bit rot is `nil, ErrPageChecksum`.

---

## Chosen layout: CRC at the end of the page span

A bbolt page is the allocated span `(overflow+1)*pageSize` with a 16-byte header (`id`, `flags`, `count`, `overflow`). **The header size does not change.**

```
[ 16-byte Page header ]
[ elements + keys + values … ]
[ CRC-32 Castagnoli, 4 bytes ]   ← last 4 bytes of the span
```

The CRC covers the **header and payload**: bytes `[0 : spanLen-4]`. Only the trailing CRC field itself is excluded.

**Which pages get a trailing CRC**

| Page type | Trailing page CRC | Why |
|--|--|--|
| Leaf / branch (and overflow) | Yes | This is the `Get`/`Put` data path. |
| Meta (pages 0 and 1) | **No** | `Meta` already has FNV-1a (`Meta.checksum` / `Validate`). A second CRC at the end of the page is redundant and would sit outside the struct the rest of the code already checks. |
| Freelist | **Not yet** | Not on the `Get` path. Left out for now; can be added later without changing the leaf/branch layout. |

New files are **version 3** and stamp this CRC on leaf/branch writes. **Version 2** files still open (meta validate accepts 2 and 3) with **no page CRC and no verify**. Upgrading a v2 file to v3 — rewrite reachable branch/leaf pages with trailing CRCs and bump `meta.version` — will be done later by **scrub**, not in this change. Until then, `Get`/`Put` assume a freshly created v3 database.

### Why the end of the span (not the header)

Putting `checksum` in the `Page` struct (after `overflow`) grows the header **16→24 bytes**. Every element and key/value then starts 8 bytes later.

That breaks existing version-2 files unless the engine supports **two header sizes** at once: read v2 at offset 16, write v3 at offset 24. Until every reachable page is rewritten, a mixed tree is unreadable. In-place “just add a field” is impossible; the payload must move.

CRC at the **end of the span** keeps:

- `id` / `flags` / `count` / `overflow` at the same offsets as v2
- elements at byte 16 (`PageHeaderSize` unchanged)
- `Key()` / `Value()` as contiguous mmap slices (no holes in overflow data)

If the packed payload would occupy the last 4 bytes, allocate one extra overflow page (or leave slack), then stamp the CRC.

CRC in the last 4 bytes of **every** 4KiB physical block was considered and dropped: those bytes sit in the middle of overflow values, so packing and `Get` would have to skip holes.

### Why not a sidecar file

A sidecar would keep checksums in a second file (or a reserved region) keyed by pgid, leaving the 16-byte header and payload layout untouched.

We did not choose that:

- **Extra I/O on every page write.** Copy-on-write already issues a new page `writeAt` plus `fdatasync`. A sidecar needs another write (and, for crash safety, another sync) whenever a page CRC changes. That doubles metadata durability work next to the path that already dominates commit latency.
- **Two files, one commit.** Meta commit today is one atomic pointer (pages 0/1). A sidecar can be ahead or behind the data file after a crash: CRC for a pgid that was reused, or a live page with a stale CRC. Repairing that needs its own WAL or a two-phase protocol. bbolt’s durability model is “data pages then meta”; a sidecar does not fit that.
- **Lifetime of pgids.** Freed pages are reused. The sidecar must delete or overwrite CRCs in lockstep with the freelist. Misses look like bit rot or hide real rot.
- **Operations.** Backup, `CopyFile`, compact, and mmap tooling all assume one data file. A second file is easy to omit, get out of date, or point at the wrong DB.

In-file CRC at the end of the span is updated in the **same** `writeAt` as the page bytes. After meta sync, readers see data and CRC together. Version 2 stays a single file with no sidecar to ignore.

---

## Detection strategy (what we verify, when)

```mermaid
flowchart LR
  A1[A self-CRC on Get]
  A2[B Merkle on Get]
  A3[C Check only]
```

**Decision: Approach A** — self-CRC on the page span, verify on every read used by `Get` / cursor. B and C remain alternatives. Layout is end-of-span CRC, not a header field.

The parent does **not** store the child’s CRC. Meta’s FNV checksum still covers only the commit pointer (root pgid, freelist pgid, high-water mark, txid).

**Catches:** flipped bits in a span you actually read (header `id`/`flags`/`count`/`overflow`, keys, values, child pgids). A rotten pgid inside a branch fails when that **branch** span’s CRC is checked.

**Misses:** a CRC-valid page in the wrong slot (copy P over Q, lost write, reuse). `Get`/`Check` stay silent; Approach B would catch it.

---

## Approach B — Merkle page tree, verify on every read

Approach A, plus:

- Each `branchPageElement` stores the child’s page CRC.
- Meta stores `rootChecksum` and `freelistChecksum` (covered by existing meta FNV).
- On Get: verify self-CRC, then **child CRC == parent slot**. Root CRC must equal meta.

**Not in this work.** Stronger against swapped pages; larger format break (branch elements grow).

---

## Approach C — CRC stored, verify only on `Check` / scrub

Same on-disk CRC as A, but **not** checked in `Tx.page`. `Get` stays as fast as today. Detection only when `Tx.Check`, `bbolt check`, or a scrub runs.

**Not the Get path.** Production rarely runs `Check` on the latency path, so rotten `Get` would stay silent between scrubs.

---

## Comparison

| | A Self-CRC on Get | B Merkle on Get | C Check only | Sidecar CRC |
|--|--|--|--|--|
| Detect rot on `Get` | Yes (spans touched) | Yes (spans + parent binding) | No | Yes if sidecar is read on Get |
| Swapped valid page | No | Yes | Only if Merkle stored | No (unless Merkle in sidecar) |
| Header size | Unchanged (16) | Unchanged if CRC stays at span end | Same as A | Unchanged |
| Extra files | No | No | No | Yes |
| Extra write/sync per page | No (CRC in same `writeAt`) | No | No | Yes |
| Crash atomicity | Same as today (data then meta) | Same | Same | Sidecar vs data file can diverge |
| v2 open | Skip verify | Skip verify | Skip verify | Sidecar absent or ignored |
| Get latency | CRC × height | CRC × height | Unchanged | CRC × height + sidecar lookup |

---

## Performance (Approach A)

Assumptions: 4KiB pages, CRC-32 Castagnoli over the span except the last 4 bytes, B+tree height **h ≈ 2–5**. Hashing 4KiB in Go is well under a microsecond for CRC-32C. `fdatasync` and mmap faults dominate writes and cold reads.

- **Get:** CRC × **h** spans on the path (cache per Tx so `seek` does not rehash the root every time). Overflow: hash the whole span once.
- **Cursor.Next:** CRC one leaf when crossing pages if the path is already cached.
- **Put + Commit:** one CRC per spilled span before `writeAt`. Cheap next to two `fdatasync`s. No second file.
- **Check:** extra CPU ≈ size / CRC throughput; usually still I/O-bound.

Without a per-Tx verified-pgid set, cursor-heavy workloads would rehash the root on every `Get`. That cache is required.

---

## What we will implement

- File format **version 3** for new databases. `Page` header stays 16 bytes. CRC-32 Castagnoli in the last 4 bytes of leaf/branch spans.
- Hash header + payload (`[0 : spanLen-4]`); the trailing 4-byte CRC is not hashed.
- Stamp CRC on leaf/branch write before `writeAt`. Verify those pages on `Tx.page` / `Get`. `View`/`Update` return `ErrPageChecksum`.
- **Meta pages:** existing FNV only; no trailing page CRC.
- **Freelist pages:** no trailing page CRC for now.
- Version 2: still **opens** (no CRC verify). **Upgrade to v3 is via scrub** (future).

**Out of scope for this change:** `GetWithError` / `Cursor.Err`, Merkle, sidecar, header CRC, per-4KiB CRC holes, compact-as-upgrade, scrub itself, freelist page CRC.
