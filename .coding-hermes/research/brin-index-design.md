# BRIN-like Sparse Secondary Index Design

Status: design only (Phase 2c)

Scope: add a BRIN-like sparse secondary-index type to the RethinkDB tree at
`/home/kara/rethinkdb`. This document intentionally specifies no implementation
patches. It is the design companion to the preceding vector-index design work;
its build-and-persist sidecar follows the VECTOR-7b HNSW pattern, while its
public configuration follows the established multi, geo, FTS, and vector
secondary-index pattern.

## 1. Overview — what BRIN brings to RethinkDB

A conventional RethinkDB secondary index stores an entry for every indexed row.
That makes point lookup and arbitrary attribute ranges efficient, but carries
per-row B-tree storage and write-maintenance cost. A BRIN-like index instead
stores one min/max summary for a *primary-B-tree key-space range* containing
many physically adjacent rows. Query execution first rejects ranges whose
summary cannot overlap the requested predicate, then scans only surviving
primary-key ranges and applies the exact predicate to every candidate row.

The key distinction is important: “block range” must mean a contiguous range in
the table's ordered primary-key B-tree, not a range of already-sorted secondary
values. Grouping the ordinary sindex by its own indexed value would reproduce a
full per-row index and make its min/max summaries largely redundant. This design
therefore summarizes indexed attribute values over contiguous `store_key_t`
(primary-key) ranges. RethinkDB does not expose storage pages as a public
abstraction, but it does expose ordered B-tree keys and `key_range_t`; those are
the correct database-internal analogue of PostgreSQL BRIN page ranges.

This index is particularly effective when the selected attribute is naturally
correlated with primary-key order or physical insertion/order locality:

- time-series tables whose primary keys or insertion order are sequential and
  whose indexed attribute is a timestamp;
- sequential IDs, monotonically increasing event numbers, and dates;
- ZIP/postal codes or other low-churn, locality-preserving categorical values.

It is deliberately not a replacement for a normal sindex. Tables with random
UUID primary keys and uncorrelated timestamps will produce broad summaries;
those queries may scan most or all ranges and receive little or no benefit.

Design invariants:

1. BRIN is exact, never approximate: a summary may create false positives, but
   must never exclude a row satisfying the ReQL range predicate.
2. BRIN avoids a per-row secondary B-tree. Its persistent footprint is O(R × C),
   where R is the number of primary-key ranges and C is the number of summarized
   columns, instead of O(N) for N indexed rows.
3. A BRIN index is a distinct sindex kind. It shares creation, metadata,
   readiness, authorization, and lifecycle machinery with existing sindexes,
   but has a specialized range-read path over the primary B-tree.
4. Phase 1 supports scalar, orderable values and range predicates only. It does
   not silently fall back to an incomplete interpretation of multi, geo, FTS,
   vector, or `getAll` behavior.

## 2. API Design — index creation optargs and querying approach

### Creation

BRIN is created through the existing `indexCreate` term, with a new `brin`
object optarg. The supplied deterministic index function remains authoritative
for the value summarized and later rechecked. `columns` is persisted as a
human-readable schema declaration and establishes the number/order of summary
components; in Phase 1 it must contain exactly one string.

```javascript
// Canonical timestamp example
r.table("events").indexCreate(
  "created_at_brin",
  r.row("created_at"),
  {brin: {columns: ["created_at"]}}
)

// Explicit range target (number of primary-B-tree rows per logical range)
r.table("events").indexCreate(
  "created_at_brin_256",
  r.row("created_at"),
  {brin: {columns: ["created_at"], range_size: 256}}
)

// Sequential-ID example
r.table("orders").indexCreate(
  "sequence_brin",
  r.row("sequence"),
  {brin: {columns: ["sequence"], range_size: 128}}
)
```

`range_size` is optional and has a server-defined default of `128` live rows per
initially constructed logical range. It must be a finite integral ReQL number
in `[16, 65536]`; reject booleans, fractions, arrays, and strings. The default
is intentionally expressed in rows rather than storage pages because RethinkDB
does not expose stable page ranges. It is a construction/maintenance target,
not a promise that every long-lived range stays exactly that size.

Validation rules:

- `brin` must be an object containing `columns`.
- `columns` must be a non-empty `ARRAY` of valid, non-empty `STRING` names.
- Phase 1 requires `columns.size() == 1`; reject a larger list with a clear
  “multi-column BRIN is not implemented” error rather than persisting a config
  the executor cannot evaluate correctly.
- The index function must be deterministic, as it already is for all sindexes.
- Each non-null function result must be an orderable scalar datum supported by
  existing `ql::datum_t::cmp`: number, string, bool, time, binary, or a ReQL
  sentinel. Arrays, objects, geometry, and vectors are rejected/omitted using
  the ordinary sindex function-error policy; they never enter a summary.
- `{multi: true}`, `{geo: true}`, `{fts: true}`, and `{vector: ...}` are
  mutually exclusive with `{brin: ...}` in Phase 1. A BRIN summary cannot
  correctly represent the current multi-key, geospatial, token, or k-NN
  semantics without a separate design.

The server returns the normal `{"created": 1}` response. `indexWait` remains
required before relying on a newly created index. `indexStatus` exposes:

```javascript
{
  index: "created_at_brin",
  ready: true,
  brin: true,
  brin_columns: ["created_at"],
  brin_range_size: 128,
  brin_ranges: 782,
  brin_dirty_ranges: 4,
  function: <binary>,
  query: "indexCreate('created_at_brin', ... {brin: {columns: ['created_at'], range_size: 128}})"
}
```

`brin_ranges` and `brin_dirty_ranges` are status observability fields, not
consistency controls. A dirty range remains queryable because its persisted
extrema are conservative.

### Querying

No new ReQL term is needed for Phase 1. Existing `between` becomes BRIN-aware
when its `index` is a BRIN index:

```javascript
r.table("events")
 .between(r.time(2026, 7, 1, "Z"), r.time(2026, 7, 2, "Z"),
          {index: "created_at_brin", leftBound: "closed", rightBound: "open"})

r.table("orders")
 .between(9_000_000, 9_999_999, {index: "sequence_brin"})
```

The planner identifies the index type from its stored `sindex_disk_info_t`.
For a BRIN index it reads summaries, emits only primary-key ranges whose
`[min_value, max_value]` overlaps the requested `ql::datum_range_t`, scans
those primary ranges, evaluates the persisted index function for each row, and
applies the same `datumspec.contains(value)` check used by ordinary sindex
range reads. That final recheck preserves open/closed bounds, ReQL collation,
and exactness.

Phase 1 non-goals for the public API:

- `getAll(..., {index: brin})` is rejected. BRIN is range-pruning machinery,
  not a point-value access path.
- `orderBy({index: brin})` is rejected. BRIN traverses primary-key ranges and
  cannot guarantee secondary-value order without a materializing sort.
- `getIntersecting`, `getNearest`, `vectorNear`, multi-index values, compound
  indexes, and changefeed initial scans do not use BRIN.
- The existing `between` semantics and return shape remain unchanged. Its
  default unordered behavior needs no secondary-value ordering guarantee.

## 3. Data Structures — summary types, range partitioning, storage layout

### Configuration types

The following exact C++ fields extend the existing types. They use the same
naming and `enum class` style as `sindex_multi_bool_t`, `sindex_geo_bool_t`,
`sindex_fts_bool_t`, and `sindex_vector_bool_t`.

```cpp
enum class sindex_brin_bool_t { REGULAR = 0, BRIN = 1 };

// Added to sindex_config_t
sindex_brin_bool_t brin;
std::vector<std::string> brin_columns;
uint64_t brin_range_size;

// Added to sindex_disk_info_t
sindex_brin_bool_t brin;
std::vector<std::string> brin_columns;
uint64_t brin_range_size;
```

`brin_range_size` uses `uint64_t`, not `size_t`, because it is durable/wire
metadata and must have a stable width across 32- and 64-bit builds. Its zero
value means “not a BRIN index” and is the deserialization default for older
metadata. A valid BRIN config always records a non-zero validated size.

The enum receives:

```cpp
ARCHIVE_PRIM_MAKE_RANGED_SERIALIZABLE(
    sindex_brin_bool_t, int8_t,
    sindex_brin_bool_t::REGULAR, sindex_brin_bool_t::BRIN);
```

### Persistent summary types

A BRIN sidecar holds a sorted vector of these records in a serialized blob. The
canonical implementation type is:

```cpp
struct brin_summary_t {
    store_key_t primary_key_left;
    key_range_t::right_bound_t primary_key_right;
    std::vector<ql::datum_t> minimum;
    std::vector<ql::datum_t> maximum;
    uint64_t live_row_count;
    uint64_t null_row_count;
    bool dirty;
};

struct brin_index_t {
    uint32_t format_version;
    uint64_t range_size;
    std::vector<std::string> columns;
    std::vector<brin_summary_t> summaries;
};
```

`primary_key_left` and `primary_key_right` form the exact `key_range_t` to scan;
using `key_range_t::right_bound_t` preserves RethinkDB's unbounded and
open/closed primary-key boundary representation. `minimum[i]` and `maximum[i]`
are full `ql::datum_t` values, not truncated B-tree encodings, so summary
comparison uses the same datum collation (`datum_t::cmp`) as ReQL. `minimum`
and `maximum` have exactly `columns.size()` members. In Phase 1 that is one.

`live_row_count` is a diagnostic/planning estimate. `null_row_count` counts
rows whose mapping returned `null` or no indexable value; nulls are never
claimed to satisfy a normal `between` predicate. `dirty` means a delete or
update made the stored extrema wider than a fresh rescan would produce. It
never means the summary is unsafe.

`brin_index_t` is written with explicit versioned serialization using the same
`serialize_onto_blob<cluster_version_t::LATEST_DISK>` mechanism used for the
HNSW graph, with `ql::datum_t` serialized through `rdb_protocol/serialize_datum.hpp`.
It is intentionally not stored as one `secondary_index_t` record per range:
that would bloat the primary sindex catalog and make range enumeration
inefficient.

### Range partitioning

At post-construction, traverse the primary B-tree in ascending `store_key_t`
order. Start a new summary after `brin_range_size` live primary rows. Each
summary captures the inclusive/exclusive key boundaries of that traversal
segment and min/max values produced by the configured function. The resulting
logical ranges are disjoint, sorted, and cover the primary key universe; empty
spans are not stored.

Ranges are key-space ranges rather than fixed numeric intervals. This lets the
design work with numeric, string, UUID, and composite encoded primary keys,
while still matching the B-tree's ordered layout. The initial row-count target
is only a proxy for physical block density. Future page-aware work may replace
that grouping heuristic without changing the ReQL API or summary format version.

Ranges do not split synchronously on each insert. An insert expands its
containing summary (or creates a summary in a previously empty span), and a
range that grows beyond `2 * range_size` is marked dirty for asynchronous
repacking. This keeps write latency bounded and mirrors BRIN's intentionally
coarse granularity. A rebuild/repack creates a fresh sorted summary blob and
atomically swaps its sidecar block ID.

### Storage layout

Each BRIN sindex still owns the normal `secondary_index_t` catalog record,
including its UUID, readiness state, opaque definition, and sindex superblock.
However, it does **not** populate that sindex B-tree with one entry per row.
A new `block_id_t brin_summary_block` is added to the sindex superblock's
on-disk layout beside the existing `vector_graph_block`. `NULL_BLOCK_ID` means
an absent/empty sidecar; an empty index may validly have that value.

The durable hierarchy is:

```text
table primary superblock
└── secondary_index_t (name, UUID, opaque_definition, construction state)
    └── sindex superblock
        └── brin_summary_block -> blob(brin_index_t)
```

The sidecar is intentionally one compact blob rather than a B-tree of summary
rows. A typical 100-million-row table with `range_size = 128` has roughly
781,250 summaries. That is already vastly less metadata than 100 million
secondary entries; if that blob becomes too large for practical replacement,
Phase 2 may shard `brin_index_t::summaries` into a small summary B-tree without
changing the configuration or execution semantics.

## 4. Integration Points — where BRIN plugs into existing sindex infrastructure

The following locations were inspected in the current RethinkDB checkout
(`95de3f0306`, rooted at `/home/kara/rethinkdb`). Line ranges identify the
implementation seam, not a promise that nearby code remains unchanged.

| Area | Existing proof/pattern | BRIN design change |
| --- | --- | --- |
| Index-kind enum/config | `src/rdb_protocol/context.hpp:65-104` declares four type flags, ranged enum serializers, `sindex_config_t`, and `RDB_DECLARE_SERIALIZABLE`. | Add `sindex_brin_bool_t`, its ranged serializer, and the three config fields. Default constructor values must deserialize as regular/non-BRIN. |
| Config equality and wire metadata | `src/rdb_protocol/context.cc:13-33` compares config fields and implements versioned config serialization. | Include BRIN kind, columns, and range size in equality and an additive, version-gated config serialization change. Do not blindly extend the current historical `_4_SINCE_v2_1` layout for old readers. |
| Disk index description | `src/rdb_protocol/btree.hpp:260-297` defines `sindex_disk_info_t`; `src/rdb_protocol/btree.cc:1613-1738` serializes and additively deserializes FTS/vector fields. | Mirror the fields in disk info, append them after vector fields, and default them when the stream ends, exactly as FTS/vector do. |
| ReQL creation/status | `src/rdb_protocol/terms/sindex.cc:26-97`, `101-185`, and `188-288` serialize config, print status/query text, declare optargs, initialize defaults, and validate vector options. | Add `brin` to `optargspec_t`; parse/validate its object; populate config; render `brin`, `brin_columns`, and `brin_range_size` in status and reconstructed query text. |
| Catalog creation | `src/rdb_protocol/btree_store.cc:365-412` constructs disk info, stores an opaque definition, creates the catalog record, and starts post-construction. | Pass BRIN fields into disk info. Reuse this lifecycle; do not introduce a separate DDL pathway. |
| Catalog lifecycle | `src/btree/secondary_operations.hpp:25-70` defines the catalog record, UUID defense, readiness state, and opaque definition. | Reuse `secondary_index_t` unchanged; BRIN's range summaries belong in its superblock sidecar, not in the catalog record. |
| Superblock sidecar | `src/btree/reql_specific.hpp:48-75` exposes the existing vector block; `src/btree/reql_specific.cc:13-25` defines the packed disk layout and `:143-197` accesses/initializes it. | Add `block_id_t brin_summary_block`, getter/setter, initialization to `NULL_BLOCK_ID`, and adjust packed-layout capacity/accounting with a disk-format compatibility plan. |
| Construction lifecycle | `src/rdb_protocol/protocol.cc:61-208` runs durable post-construction and then invokes VECTOR-7b HNSW building. | After the primary/sindex construction loop, call `build_and_persist_brin_summaries_for_sindex` for BRIN indexes, in the same fresh-transaction/tolerant style. |
| Existing vector sidecar model | `src/rdb_protocol/protocol.cc:289-395` validates kind, depth-first traverses, writes a serialized blob, and saves its block ID. | Use this as the direct model for BRIN summary blob allocation, atomic publication, empty index handling, and concurrent drop checks. |
| Live write maintenance | `src/rdb_protocol/btree.cc:1765-1970` derives sindex keys and writes/deletes per-row entries; `:1973-2009` filters incomplete construction ranges. | Branch on `sindex_info.brin`: maintain the affected summary under the same transaction rather than writing a normal sindex entry. Never perform both operations for one BRIN index. |
| Range read protocol | `src/rdb_protocol/protocol.hpp:278-364` defines `sindex_rangespec_t` and `rget_read_t`; `src/rdb_protocol/store.cc:228-283` dispatches ordinary sindex scans. | Add a serializable BRIN range-read specification/variant carrying the index name, `ql::datumspec_t`, and normal batch/transform state; dispatch it to a primary-B-tree scan of selected ranges. |
| Table/readgen entry points | `src/rdb_protocol/real_table.cc:39-104` selects primary vs sindex readers; `src/rdb_protocol/datum_stream.cc:1170-1213` packages an ordinary sindex `rget_read_t`. | Preserve the caller-facing `between` API, but make the readgen/planner select the BRIN variant after index metadata is resolved. |
| ReQL bounds | `src/rdb_protocol/terms/seq.cc:678-724` creates `datum_range_t` for `between`; `src/rdb_protocol/datumspec.cc:57-108` implements exact datum range membership and ordinary sindex encoding. | Use the unencoded `datum_range_t` for BRIN summary overlap and final value recheck. Do not compare truncated `store_key_t` encodings to summaries. |

The multi, geo, FTS, and vector flags in `context.hpp`/`sindex.cc` are the
proven configuration pattern. Geo's specialized range execution in
`store.cc:257-283` is the precedent for rejecting incompatible generic reads.
Vector's HNSW sidecar is the proven post-construction storage pattern. BRIN
combines those patterns but never reuses HNSW's approximate-search semantics.

## 5. Build Pipeline — construction during and after sindex creation

1. **Parse and persist configuration.** `indexCreate` validates `{brin: ...}`
   and produces `sindex_config_t`. `store_t::sindex_create` persists the
   resulting `sindex_disk_info_t` in `secondary_index_t::opaque_definition`.
   Its normal post-construction state starts as `key_range_t::universe()`.

2. **Use normal primary-table construction semantics.** The existing
   `resume_construct_sindex` loop (`protocol.cc:114-194`) already handles
   primary-key traversal in bounded passes, modification queues, restart safety,
   post-construction progress, and concurrent writers. A BRIN index must join
   those readiness semantics; it must not become queryable before its summary
   sidecar covers the post-constructed primary-key universe.

3. **Avoid building ordinary row entries.** In the post-construction callback,
   detect BRIN and compute/accumulate summaries from each primary row instead of
   invoking the path that creates a per-row secondary `store_key_t` entry. The
   modification queue remains necessary: mutations concurrent with the initial
   traversal must be folded into the relevant summaries before publication.

4. **Build a private complete sidecar.** Once construction covers the universe,
   acquire the sindex catalog and its superblock in a fresh HARD-durability
   transaction. Re-read `opaque_definition`, verify `brin == BRIN`, traverse the
   primary B-tree in key order, and create an in-memory `brin_index_t`.

5. **Publish atomically.** Serialize `brin_index_t` to a new child blob block,
   write its block ID to `brin_summary_block`, and commit. Until that commit,
   the index is not ready. If the table/index was dropped concurrently, abort,
   release acquired locks, and leave no catalog reference to the new block.

6. **Recovery and resume.** A crash before publication leaves the BRIN index
   non-ready and is retried by the existing post-construction resume mechanism.
   A crash after publication leaves a complete immutable sidecar. This mirrors
   the vector code's `NULL_BLOCK_ID`/fresh-transaction strategy but has a
   stronger readiness requirement: range reads must not treat a missing BRIN
   sidecar as an empty answer.

7. **Repack.** Dirty or oversized ranges are rebuilt asynchronously into a new
   blob. The old blob continues serving readers until the sidecar pointer swap
   commits. A full `indexRebuild` uses the same pipeline and is the operator
   escape hatch for badly correlated data or accumulated drift.

The builder needs a testable pure helper that maps ordered `(store_key_t,
ql::datum_t)` rows to `brin_index_t`. Storage code then owns locking, traversal,
blob writing, and atomic pointer publication.

## 6. Query Execution — how `between` uses BRIN summaries

For `table.between(left, right, {index: "x"})`, `between_term_t` already
constructs a `ql::datum_range_t` with the caller's bound types. The BRIN plan is:

1. Resolve index `x` and deserialize its `sindex_disk_info_t`.
2. Reject incompatible BRIN calls (`getAll`, geo/vector APIs, ordered sindex
   access) before reading storage.
3. Read the immutable `brin_index_t` blob referenced by the index superblock.
   Missing/null sidecar on a non-ready index follows the normal “index is not
   ready” path; it never returns an empty result.
4. For each summary, apply the conservative overlap predicate:

   ```text
   candidate iff summary.maximum[0] is not strictly below query.left
             and summary.minimum[0] is not strictly above query.right
   ```

   The concrete implementation must account for `key_range_t::open`/`closed`
   bounds using `datum_range_t::contains`-equivalent comparison, rather than
   simplifying it to numeric `<=`. A summary containing no indexable values
   cannot match and is skipped.

5. Merge adjacent candidate primary-key ranges to reduce traversal overhead.
6. Issue primary B-tree `rdb_rget_slice` work for each merged range, honoring
   current shard boundaries, batch limits, transforms, terminal operations, and
   cancellation.
7. For every row read, invoke the persisted deterministic mapping function and
   apply `datumspec.contains(mapped_value)`. Emit only exact matches.
8. Preserve normal cancellation, error propagation, and deduplication rules.
   Candidate ranges are non-overlapping, so no row is emitted twice.

For an unbounded `between(r.minval, r.maxval, {index: brin})`, the planner may
bypass summaries and scan the primary table directly; BRIN cannot prune that
query. For a range whose summaries all overlap, the plan degenerates safely to
a full primary scan. The query optimizer should record profile counters:
`brin_ranges_total`, `brin_ranges_selected`, `brin_primary_rows_scanned`, and
`brin_rows_returned`, making poor correlation visible rather than mysterious.

Sharding: summaries are local to each shard's primary B-tree. The coordinator
sends the same datum range to each shard; each shard prunes against its own
sidecar and scans only local candidate `key_range_t`s. Cross-shard result merging
uses existing unordered range-read behavior. No global summary and no
cross-shard physical ordering are required.

## 7. Serialization — wire protocol and disk format

### Configuration serialization

`context.hpp:70-104` is the declaration site for the ranged enum and
`sindex_config_t`; `context.cc:32-33` is its serialized implementation. BRIN
must be an additive, versioned extension:

- old metadata deserializes as `brin = REGULAR`, `brin_columns = {}`, and
  `brin_range_size = 0`;
- a BRIN-capable reader must not send BRIN config to an older cluster member;
  cluster-version gating follows the project's standard compatibility release
  procedure;
- equality includes all three BRIN fields, preventing configuration changes
  from being ignored by semilattice/table metadata logic.

The present config implementation serializes only its historical fields, while
disk index info already performs optional-tail decoding for newer FTS/vector
metadata. The implementation must explicitly reconcile this before shipping:
append BRIN config fields in a version-gated format or make cluster metadata
carry the opaque definition consistently. Do not assume a declaration field is
wire-compatible merely because `RDB_DECLARE_SERIALIZABLE(sindex_config_t)`
exists.

### Opaque sindex definition

`serialize_sindex_info` (`btree.cc:1613-1630`) appends fields after the mapping,
reQL versions, multi, geo, FTS, and vector data. Append, in this exact order:

```text
brin (sindex_brin_bool_t)
brin_columns (std::vector<std::string>)
brin_range_size (uint64_t)
```

`deserialize_sindex_info` first assigns safe defaults, then reads the complete
BRIN tail only if bytes remain. It must reject a truncated partial tail rather
than treating a partially read BRIN index as regular. This is the durable
configuration stored in `secondary_index_t::opaque_definition`.

### Summary sidecar

The `brin_index_t` blob begins with `uint32_t format_version = 1`. The serializer
writes fields in the declaration order shown in Section 3. For each
`brin_summary_t`, it serializes primary-key boundaries, vector lengths and
strings/datums, counts, and `dirty`. Readers validate:

- format version is supported;
- `range_size > 0` and equals the configured range size;
- persisted `columns` equals configured columns;
- `minimum.size() == maximum.size() == columns.size()`;
- ranges are sorted and non-overlapping;
- every `minimum[i] <= maximum[i]` under `datum_t::cmp`.

Validation failure makes the index unavailable with a recoverable “rebuild the
index” error; it must not produce a potentially false-negative answer.

### Superblock evolution

Adding `brin_summary_block` changes packed `reql_btree_superblock_t` in
`src/btree/reql_specific.cc:13-25`. Because this is disk layout, it requires a
new storage-format/version migration and careful `METAINFO_BLOB_MAXREFLEN`
accounting. The block must be initialized to `NULL_BLOCK_ID` in
`init_sindex_superblock`, and index drop/clear must release it just as the
vector graph child is released. This compatibility work is a hard dependency,
not cleanup to defer after query logic lands.

## 8. Edge Cases — empty ranges, single-element ranges, deletes, updates

| Case | Required behavior |
| --- | --- |
| Empty table/index | Publish either an empty `brin_index_t` or `NULL_BLOCK_ID` with ready status only when the read path can distinguish ready-empty from unready. Any BRIN `between` returns an empty sequence. |
| Single indexed row | One summary has one key-space range, identical minimum/maximum, `live_row_count = 1`; equal closed bounds select it, either open bound excludes it. |
| Null/missing mapped value | Increment `null_row_count`; do not update min/max. A range containing only nulls has no candidate values and is skipped for ordinary `between`. |
| Non-indexable function result | Follow normal sindex function failure/omission behavior; never serialize incomparable min/max values. Surface a creation/build error when the configured function cannot produce Phase-1-compatible values. |
| Delete of non-extremum | Decrement `live_row_count` where possible; summary remains correct. |
| Delete of current min/max | Do not shrink extrema synchronously. Keep the old boundary, set `dirty = true`, and schedule repack. This may scan extra rows but cannot lose matches. |
| Update value widens range | Expand min/max atomically in the summary transaction. |
| Update value narrows range | Keep old extrema and mark dirty; asynchronous rebuild recomputes exact extrema. |
| Update primary key / relocation | Treat as delete from old key-space range plus insert into new range within the same write ordering. The ordinary mutation report already includes deleted and added values (`btree.cc:1799-1928`). |
| Insert into a populated span | Locate the containing summary by key boundary; update its extrema/count. If it grows beyond the repack threshold, mark dirty. |
| Insert into an empty primary-key span | Create a summary only if the complete sidecar range partition can remain sorted/non-overlapping; otherwise mark the index for rebuild. Never leave a key-space gap that could cause a false negative. |
| Index build concurrent with writes | Reuse the existing post-construction modification queue and do not mark ready until all queued changes are folded in. |
| Concurrent drop | Recheck catalog UUID/`being_deleted` before sidecar publication; abandon unpublished block on drop. |
| Interrupted build/restart | Existing `needs_post_construction_range` resumes construction. No partially built sidecar is readable. |
| Corrupt/missing summary blob | Return a recoverable index failure/rebuild-required error, not an empty result or full result whose correctness is unknown. |
| Random/unrelated data | Summaries overlap most predicates; produce a safe primary scan and expose poor selectivity via profile/status counters. |
| Range boundary/truncated keys | Summary predicates use full `ql::datum_t`; primary scans use stored `key_range_t` endpoints. Do not use ordinary truncated secondary-key conversion for BRIN summaries. |

## 9. Performance Characteristics — expected index size and query speedup

Let N be live rows, C summarized columns (C = 1 in Phase 1), and K be
`range_size`. The initial number of summaries is approximately `ceil(N / K)`.
A conventional RethinkDB sindex needs one secondary B-tree key/value entry per
indexed row. BRIN needs two datum extrema plus two key boundaries and counters
per logical range.

For the default K = 128, the index has about 0.78% as many logical records as a
row-per-entry index. With a 64–256 row range target, it is normally expected to
be roughly 100–1000× smaller than an equivalent ordinary sindex, although the
actual byte ratio depends on primary key width, datum type, compression/blob
overhead, mutation churn, and number of dirty/repacked ranges. That ratio is a
targeted expectation, not a storage guarantee to expose as an API contract.

For well-correlated timestamps, a narrow time predicate selects a small,
contiguous fraction of summaries and then scans only their underlying primary
ranges. If S summaries survive, expected work is O(R + S × K + matches), where
R is the summary count for a single blob scan. The summary pass is sequential,
cache-friendly, and tiny relative to a normal sindex. A future summary B-tree
can reduce large-R selection cost if the sidecar itself becomes substantial.

For uncorrelated data, most summaries have min/max that span the requested
value, S approaches R, and the query scans most rows. It remains correct but
can be slower than an ordinary sindex due to mapping-function rechecks. The
planner should not promise automatic selection: using `{index: "..._brin"}` is
an explicit operator choice in Phase 1, and profiling documents the result.

Write cost is O(1) summary lookup/update in the normal case, plus occasional
asynchronous O(K) range repacks or O(N) rebuild. It avoids per-row secondary
B-tree insertion/deletion but introduces summary blob copy/update costs. The
implementation should benchmark write-heavy workloads before claiming a
universal write-throughput improvement.

Recommended benchmarks:

1. 10M time-ordered events, primary key correlated with timestamp; compare
   ordinary sindex vs BRIN bytes, build time, narrow 1-minute/1-day ranges.
2. Same data with random UUID primary keys; quantify selected-range percentage
   and demonstrate controlled degradation.
3. Sequential order IDs and ZIP-code ranges.
4. Delete/update churn that repeatedly removes extrema; verify correctness and
   dirty-range/repack behavior.
5. Multiple shards; verify that per-shard pruning aggregates correctly.

## 10. Implementation Plan — phases, dependencies, estimated complexity

### Phase BRIN-1: configuration and compatibility foundation — Medium

- Add the enum and config/disk fields at the locations in Section 4.
- Implement optarg validation, status rendering, query reconstruction, equality,
  and additive serialization defaults.
- Define one cluster/disk compatibility version and migration behavior before
  any feature code is merged.
- Unit-test valid/invalid `{brin: ...}` forms, serialized round trips, old-tail
  defaults, and reconstructed `indexStatus().query`.

Dependency: none. Exit criterion: a BRIN config can safely be created, stored,
reported, and exchanged only between compatible nodes, even though it cannot
query yet.

### Phase BRIN-2: superblock sidecar and pure summary codec — High

- Add the BRIN sidecar block pointer with correct allocation, initialization,
  deletion, migration, and crash safety.
- Implement versioned `brin_summary_t`/`brin_index_t` serialization and strict
  validation.
- Implement pure summary building and overlap helpers with exhaustive bound
  tests.

Dependency: BRIN-1 and approved disk-format migration. Exit criterion: a test
can write/read an empty, singleton, and many-range summary blob and reject
corrupt/inconsistent metadata.

### Phase BRIN-3: construction and live maintenance — High

- Integrate construction with post-construction/mutation queue semantics.
- Publish sidecars atomically and preserve readiness/restart/drop behavior.
- Branch normal sindex maintenance so BRIN never receives per-row secondary
  B-tree entries.
- Implement conservative delete/update maintenance plus dirty/repack scheduling.

Dependency: BRIN-2. Exit criterion: build, restart during build, concurrent
writes, updates, deletes, and index drop preserve all summary invariants.

### Phase BRIN-4: `between` planner/executor — High

- Add a serializable BRIN range-read variant and local shard executor.
- Wire index-type resolution from the existing `between`/readgen pipeline.
- Scan merged candidate primary ranges and recheck the persisted function with
  exact `datumspec` semantics.
- Reject unsupported APIs explicitly; add profile counters.

Dependency: BRIN-3. Exit criterion: black-box ReQL tests prove equality/open/
closed ranges, no false negatives, multi-shard behavior, empty tables, and
controlled full-scan degradation.

### Phase BRIN-5: observability, repacking, and performance acceptance — Medium

- Add `indexStatus` BRIN counts, profiling counters, dirty-range scheduling,
  `indexRebuild` behavior, and benchmark coverage.
- Establish a data-driven default range size and document correlation guidance.
- Compare on-disk footprint and query latency against a normal sindex for the
  benchmark suites in Section 9.

Dependency: BRIN-4. Exit criterion: expected 100–1000× sparse-index footprint
on naturally ordered datasets is measured rather than assumed, and operators
can diagnose/rebuild poor-correlation indexes.

### Test placement and completion gates

Use existing test conventions as anchors: `src/unittest/rdb_protocol.cc` covers
sindex lifecycle/configuration, `src/unittest/rdb_btree.cc` covers B-tree sindex
updates, and vector/HNSW tests demonstrate serialized sidecar testing. Add a
focused BRIN unit test file for summary codec/overlap invariants plus protocol
and ReQL regression tests for end-to-end `indexCreate`/`indexWait`/`between`.

Before implementation completion, require:

- serialization compatibility tests for old data and new BRIN data;
- property-style tests showing every inserted matching row is returned across
  arbitrary inserts, updates, deletes, and open/closed boundaries;
- restart/drop/concurrent-build tests;
- multi-shard range tests;
- benchmark artifacts for correlated and uncorrelated data;
- normal project verification (`make test`, `make lint`, `make build`, and
  `make fmt`) in `/home/kara/rethinkdb`.

The resulting feature remains intentionally narrow: a compact, exact,
range-pruning secondary-index type for naturally ordered data—not a general
replacement for conventional RethinkDB sindexes.
