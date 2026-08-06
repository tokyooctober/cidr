# SPEC: `cidrplan` — VLSM subnet allocator

**Status:** Draft — awaiting confirmation
**Updated:** 2026-08-06
**Repo:** `C:\src\cidr` (greenfield, empty, not yet a git repo)

---

## 1. Objective

### Problem

Given a list of subsystems, how many hosts each one needs, and how much room each
one should be given to grow, produce a non-overlapping IPv4 addressing plan: one
correctly-sized CIDR block per subsystem, laid out contiguously from a starting
address. Done by hand this is slow and error-prone — off-by-two mistakes on
usable counts, misaligned blocks, and silent overlaps are all easy to make and
hard to spot.

### What we're building

A small command-line program that reads a three-column text file (subsystem name,
host count, span) and emits an allocation table: subsystem, hosts, span, usable
IP range, assigned CIDR with its total capacity, and leftover capacity.

### Algorithm (the core of the spec)

1. **Read** the input file into `(name, hosts, span)` triples.
2. **Size each block.** Span sits *outside* the two-address overhead (confirmed).
   The demand is `hosts + span` **usable** addresses; the network and broadcast
   addresses are then added on top, because they are a structural property of the
   block rather than something span is expected to absorb:

   ```
   total_needed = hosts + span + 2
   ```

   Round `total_needed` up to the next power of two to get block size `S`; the
   prefix length is `32 - log2(S)`. This guarantees `usable ≥ hosts + span` for
   every input, so span is always fully available as growth room and leftover
   capacity can never go negative.

   No floor on span is needed. `span = 0` is valid and simply means "no growth
   room": the `+2` still applies, so the block always has room for its own
   network and broadcast addresses.
3. **Sort descending by block size `S`** (largest block first). This is not
   cosmetic: allocating largest-first is what guarantees every block lands on its
   natural power-of-two boundary, so the plan is contiguous with zero alignment
   gaps.

   **Ties are broken by input file order** — a stable sort, no secondary key. When
   two or more subsystems produce the same block size, a note is logged to stderr
   naming them, so the ordering is visible rather than silently arbitrary.
4. **Allocate sequentially** starting at the base address, advancing the cursor
   by `S` after each block.
5. **Report** each subsystem with its usable IP range (first usable → last
   usable), its CIDR block with that block's total usable capacity, and leftover
   capacity = `total_usable - (hosts + span)`.

Worked example, verified: **300 hosts + 50 span → 352 needed → 512 → `/23`**,
giving 510 usable and 160 leftover.

### Target user

Whoever is planning the address space — run locally, one file in, one table out.
Single operator, no server, no persistence.

### Success criteria

- Blocks never overlap, and every block is aligned to its own size.
- Every subsystem's usable capacity is `≥ hosts + span`.
- The example in §2 reproduces exactly, byte for byte.
- Total consumed space is minimal for the given demands (no gaps).

### Non-goals

- IPv6. The network/broadcast-address rule this spec is built on is IPv4
  semantics; IPv6 subnetting works differently. Out of scope entirely.
- Persisted state, allocation history, or reservation tracking (that's IPAM).
- Any network I/O — no pinging, DNS, scanning, or device configuration.
- Generating router/switch configs, DHCP scopes, or ACLs from the plan.
- Re-planning around already-allocated blocks, or best-fit bin packing.
  Allocation is strictly largest-first from a single starting address.

---

## ⚠️ Assumptions and interpretations

Flagged because each changes the output. Correct any that are wrong.

1. **`--base` is a bare address with no prefix length** (confirmed).
   `192.168.0.0`, not `192.168.0.0/24`. There is therefore **no bounding network
   and no overall capacity limit** — the plan simply extends upward from the base
   address as far as the demands require. Supplying a prefix is a usage error.
2. **Span is extra usable addresses on top of hosts** (confirmed). `hosts + span`
   is the demand; the `+2` for network and broadcast is added on top of *that*,
   not absorbed into span. So 300 hosts with 50 span sizes on 352, not 350.
3. **Leftover capacity = total usable − (hosts + span)** (confirmed). Span is
   claimed space, so it does not count as leftover. For Sales: 510 − 350 = 160.
4. **Sorting is by block size, not by host count.** With a span column these are
   no longer the same ordering: 100 hosts + 200 span needs a `/23`, while 150
   hosts + 0 span needs only a `/24`. Sorting by host count would put the smaller
   block first and break the alignment guarantee. Sorting by computed block size
   is the correct invariant.
5. **The base address must be aligned to the largest block.** Since the largest
   block is allocated first, an unaligned base makes that first block invalid.
   `192.168.0.0` with a `/23` first block is fine; `192.168.1.0` would not be.
   Spec'd to reject with the nearest valid addresses named — see Open Questions
   for the auto-adjust alternative.
6. **Language: Python 3.11+, standard library only** (confirmed). The built-in
   `ipaddress` module handles all address arithmetic, so this is a single file
   with no build step and no runtime dependencies.
7. **Minimum block is `/30`.** Under the network+broadcast rule, a `/31` has zero
   usable addresses. So a demand of 1 or 2 rounds up to `/30` (4 addresses, 2
   usable). RFC 3021 `/31` point-to-point links are not supported.

---

## 2. Commands

### Usage

```
python cidrplan.py <input-file> [--base ADDRESS] [--format text|csv|json]
                                [--order size|input] [--output FILE]
```

| Option | Default | Effect |
|---|---|---|
| `<input-file>` | *(required)* | Three-column input. `-` reads stdin. |
| `--base ADDRESS` | `192.168.0.0` | Starting address. A bare IPv4 address — **a prefix length is a usage error**. |
| `--format` | `text` | `text` = aligned table; `csv` and `json` for downstream tooling. |
| `--order` | `size` | `size` = allocation order (largest block first). `input` = original file order. |
| `--output FILE` | stdout | Write the report to a file instead. |
| `-q, --quiet` | off | Suppress the tie notes on stderr. Does not suppress errors. |

`--strict` from an earlier draft is **removed**. It existed to escalate duplicate
names and ties to errors; duplicates are now always rejected and ties are always
logged, so the flag had no behavior left to control.

### Input format

Three columns, one subsystem per line:

```
# subsystem      hosts  span
Sales              300     50
Engineering        120     30
Warehouse           60     10
Ops                 25      5
Guest               10      0
```

Parsing rules:

- **The last two whitespace- or comma-separated fields are hosts and span; every-
  thing before them is the name.** This lets names contain spaces
  (`HR Department  45  10`) without quoting or a configured delimiter.
- Tab, comma, or runs of spaces all work as separators.
- Lines starting with `#` and blank lines are skipped.
- `hosts` must be a positive integer. `span` must be a non-negative integer —
  zero is valid and means "no growth room". Negatives, non-numerics, and lines
  with the wrong field count are errors citing the offending line number.
- A two-column line is an error, not an implied `span` of 0. Silently defaulting
  a missing column would make a truncated file look like a valid plan.
- **Duplicate subsystem names are rejected.** Comparison is exact and
  case-sensitive, after trimming surrounding whitespace. The whole file is
  checked before any allocation happens, so a duplicate on the last line fails
  before a partial plan is printed.

### Output

**Six columns.** The first three are copied from the input file; the last three
are computed.

| # | Column | Source | Content |
|---|---|---|---|
| 1 | Subsystem | input | Subsystem name, verbatim. |
| 2 | Hosts | input | Requested host count. |
| 3 | Span | input | Requested growth room. |
| 4 | Usable IP Range | computed | First usable → last usable address, excluding network and broadcast. |
| 5 | CIDR (Total Hosts) | computed | Assigned block and its total usable capacity. |
| 6 | Leftover Capacity | computed | Column 5's capacity minus (hosts + span). |

Numeric columns (2, 3, 6) are right-aligned; text columns are left-aligned. Every
column is padded to the width of its widest cell, header included. Trailing
whitespace is stripped from each line.

Verified output for the input above, with the default base:

```
SUBSYSTEM    HOSTS  SPAN  USABLE IP RANGE                CIDR (TOTAL HOSTS)           LEFTOVER CAPACITY
Sales          300    50  192.168.0.1 - 192.168.1.254    192.168.0.0/23 (510 hosts)                 160
Engineering    120    30  192.168.2.1 - 192.168.2.254    192.168.2.0/24 (254 hosts)                 104
Warehouse       60    10  192.168.3.1 - 192.168.3.126    192.168.3.0/25 (126 hosts)                  56
Ops             25     5  192.168.3.129 - 192.168.3.158  192.168.3.128/27 (30 hosts)                  0
Guest           10     0  192.168.3.161 - 192.168.3.174  192.168.3.160/28 (14 hosts)                  4

Allocated 944 addresses from 192.168.0.0 through 192.168.3.175.
Next free address: 192.168.3.176
```

**This table is the primary acceptance test.** It must reproduce byte for byte.

Two things it demonstrates:

- **Sales spans two third-octet values** — `192.168.0.1` through `192.168.1.254`
  — because a `/23` is two `/24`s wide. The next subsystem therefore starts at
  `192.168.2.0`, not `192.168.1.0`.
- **Ops has leftover 0** — it asked for 25 + 5 = 30 and a `/27` provides exactly
  30 usable. An exact fit must not be rounded up to the next prefix.

`csv` emits seven fields — the six columns with the CIDR and its capacity split
apart (`subsystem,hosts,span,usable_range,cidr,total_hosts,leftover`) — plus a
header row and no summary. `json` emits an object with `base`, an `allocations`
array, and a `summary` object.

### Tie notes

When subsystems produce equal block sizes, allocation follows input file order and
a note goes to **stderr** — never stdout, so `csv` and `json` output stays clean
when piped:

```
note: 2 subsystems tie at /27 (Ops, Lab); allocated in input file order.
```

One note per tie group. Suppressed by `-q`. Notes never change the exit code.

### Errors

**Duplicate subsystem name** — rejected before anything is allocated:

```
$ python cidrplan.py subsystems.txt
error: duplicate subsystem name 'Sales' on line 7 (first seen on line 2).
       Subsystem names must be unique.
```

**Misaligned base** — the base must sit on a boundary of the largest block:

```
$ python cidrplan.py subsystems.txt --base 192.168.1.0
error: base 192.168.1.0 is not aligned to the largest block (/23, 512 addresses).
       Nearest valid starting addresses: 192.168.0.0 or 192.168.2.0.
```

**Base carries a prefix:**

```
$ python cidrplan.py subsystems.txt --base 192.168.0.0/24
error: --base takes a bare address, not a network. Prefix lengths are computed
       from the host and span counts. Use --base 192.168.0.0.
```

**Plan runs past the end of IPv4 space:**

```
error: plan needs 944 addresses starting at 255.255.252.0, which runs past
       255.255.255.255. Choose a lower base address.
```

### Exit codes

- `0` — plan produced successfully (tie notes do not affect this).
- `1` — plan cannot be laid out (misaligned base, or runs off the end of IPv4).
- `2` — usage or input error, including duplicate subsystem names.

### Development commands

```
python cidrplan.py examples/subsystems.txt                  # run
python -m pytest                                            # tests
python -m pytest --cov=cidrplan --cov-report=term-missing   # coverage
python -m mypy --strict cidrplan.py                         # type check
python -m ruff check .                                      # lint
python -m ruff format .                                     # format
```

---

## 3. Project structure

Deliberately flat. This is a few hundred lines of logic; a package hierarchy
would be more structure than the problem has.

```
cidr/
├── cidrplan.py           # the program: parse → size → sort → allocate → render
├── test_cidrplan.py      # pytest suite
├── examples/
│   └── subsystems.txt    # the worked example from §2
├── SPEC.md
├── README.md
└── pyproject.toml        # ruff + mypy + pytest config; no runtime deps
```

Internal organization of `cidrplan.py`, in order:

1. `Subsystem` and `Allocation` dataclasses — the two data shapes.
2. `parse_input(lines) -> list[Subsystem]` — pure, no file I/O.
3. `block_size(hosts, span) -> int` — the `+2` → next-power-of-two math.
4. `allocate(subsystems, base) -> list[Allocation]` — sort by block size, walk,
   assign. Pure. Validates base alignment.
5. `render_text` / `render_csv` / `render_json` — the only functions that format.
6. `main(argv)` — argument parsing, file I/O, exit codes.

### Rules

- **Keep the logic pure.** `parse_input`, `block_size`, and `allocate` take values
  and return values — no printing, no file access, no `sys.exit`. This is what
  makes the test suite trivial to write.
- **All address arithmetic goes through the stdlib `ipaddress` module.** No
  hand-rolled bit twiddling on integers-as-IPs, no string manipulation of dotted
  quads.
- **One file until it exceeds ~400 lines.** Split only then, and only along the
  parse/allocate/render seam.
- **Zero runtime dependencies.** Dev dependencies (pytest, ruff, mypy) are fine.

---

## 4. Code style

- **Formatting:** `ruff format` (Black-compatible) is authoritative.
- **Type hints on every function signature**, checked by `mypy --strict`. This
  domain is full of easily-confused integers and types are the cheapest guard.
- **Naming says which integer it is:** `requested_hosts`, `span`, `demand`
  (= hosts + span), `total_addresses`, `usable_addresses`, `prefix_length`,
  `leftover`. Never a bare `n`, `size`, or `count` in the allocation logic —
  `demand` and `usable_addresses` differ by exactly the padding this program
  exists to compute, and conflating them is the likeliest bug.
- **Errors:** raise `ValueError` with the line number and offending text for bad
  input; a custom `LayoutError` for alignment and overflow failures. `main` is the
  only place that catches and converts to an exit code. No bare `except:`.
- **Comments explain why.** Three places need one: the `+2`, the sort-by-block-
  size (not by host count), and the base-alignment check. All three look like
  candidates for "simplification" by someone who doesn't know why they're there.
- **No premature abstraction.** No allocation-strategy classes, no plugin registry
  for output formats — a dict of format name → render function is enough.
- **Deterministic output.** Same input, same base → byte-identical output, every
  run. Ties broken by a stable, documented rule (see Open Questions).

---

## 5. Testing strategy

`pytest`, table-driven where the cases are enumerable.

### Unit tests — `block_size`

The sizing function is where an off-by-one does the most damage. Demand is
`hosts + span`; the table covers the demand boundaries:

| Hosts | Span | Demand | Needed (+2) | Block | Prefix | Usable | Leftover |
|---|---|---|---|---|---|---|---|
| 1 | 0 | 1 | 3 | 4 | /30 | 2 | 1 |
| 2 | 0 | 2 | 4 | 4 | /30 | 2 | 0 |
| 1 | 1 | 2 | 4 | 4 | /30 | 2 | 0 |
| 3 | 0 | 3 | 5 | 8 | /29 | 6 | 3 |
| 6 | 0 | 6 | 8 | 8 | /29 | 6 | 0 |
| 14 | 0 | 14 | 16 | 16 | /28 | 14 | 0 |
| **25** | **5** | **30** | **32** | **32** | **/27** | **30** | **0** |
| 254 | 0 | 254 | 256 | 256 | /24 | 254 | 0 |
| 255 | 0 | 255 | 257 | 512 | /23 | 510 | 255 |
| 200 | 54 | 254 | 256 | 256 | /24 | 254 | 0 |
| 200 | 55 | 255 | 257 | 512 | /23 | 510 | 255 |
| 254 | 2 | 256 | 258 | 512 | /23 | 510 | 254 |
| **300** | **50** | **352** | **354** | **512** | **/23** | **510** | **160** |
| 510 | 0 | 510 | 512 | 512 | /23 | 510 | 0 |

The exact-fit rows (2, 6, 14, 30, 254, 510) are the critical ones: a `>=` vs `>`
error makes them jump a whole prefix and silently double the plan's size. The
`200+54` / `200+55` pair proves span is added *before* the rounding, not after.

The `254 + 2` row is the load-bearing one. It is the smallest case that
distinguishes sizing on `hosts + span + 2` (a `/23`, per this spec) from sizing on
`hosts + span` (a `/24`, which would yield 254 usable against a demand of 256 —
leftover of −2). If that row ever comes out `/24`, the `+2` has been folded into
span somewhere.

Leftover is never negative — a negative would mean the block is too small for the
demand, which is a bug. Assert it on every allocation, not just in tests.

### Sorting tests

- Explicitly cover the case that motivates sorting by block size rather than host
  count: a subsystem with **100 hosts + 200 span** (needs a `/23`) must be
  allocated before one with **150 hosts + 0 span** (needs only a `/24`), even
  though 150 > 100. Sorting by host count here produces a misaligned plan.
- **Tie stability:** three subsystems producing the same block size, listed in a
  non-alphabetical input order, must be allocated in that input order. Reversing
  their order in the input must reverse their order in the output — this is what
  proves the sort is stable rather than incidentally matching.
- **Tie note:** the stderr note names every member of the tie group, appears once
  per group, is absent under `-q`, and never appears on stdout.

### Property tests

Over randomly generated `(hosts, span)` lists:

- **No overlap:** every pair of allocated blocks is disjoint.
- **Sufficiency:** every allocation's usable capacity ≥ hosts + span.
- **Alignment:** every block's network address is a multiple of its block size.
- **Contiguity:** with largest-first ordering, zero gaps between consecutive
  blocks.
- **Minimality:** no allocation could use a longer prefix and still fit.
- **Leftover consistency:** `leftover == usable - hosts - span`, always ≥ 0.

### Golden test

The §2 example table, compared byte for byte. Covers parsing, sizing, sorting,
allocation, alignment, column padding, and rendering in one shot — including the
octet-crossing `/23` and the exact-fit zero-leftover row.

### Error-path tests

- Misaligned base (`--base 192.168.1.0` with a `/23` first block) → exit 1,
  message names both nearest valid addresses. Golden test on the message text.
- `--base` given with a prefix (`192.168.0.0/24`) → exit 2, message explains that
  prefixes are computed.
- Plan runs past `255.255.255.255` → exit 1.
- Two-column line (missing span) → exit 2, correct line number.
- `span` negative → exit 2. `span` zero → valid.
- `hosts` of `0`, `-5`, `abc`, or empty → exit 2, correct line number.
- Empty input → empty table, exit 0, no crash.
- Single subsystem → works.
- Names containing spaces → parsed correctly.
- Duplicate subsystem names → exit 2, message names the subsystem and both line
  numbers, and **nothing is printed to stdout** — the rejection happens before
  allocation, so no partial plan escapes.
- Duplicate on the last line of the file → still rejected before any output.
- Names differing only in case (`Sales` / `sales`) → both allowed; comparison is
  case-sensitive.
- Names differing only in surrounding whitespace → treated as duplicates.

### Gates

- **100% line coverage on `block_size` and `allocate`.** They are small, pure, and
  carry all the risk — there is no excuse for a gap.
- ≥90% overall.
- `mypy --strict` and `ruff check` clean.
- No test touches the network or writes outside `tmp_path`.

---

## 6. Boundaries

### Always do

- Compute usable addresses as `block_size - 2`, never as `block_size`.
- Size on `hosts + span + 2`, and round up *after* adding span.
- Sort by computed block size descending — never by host count.
- Validate that the base address is aligned to the largest block before
  allocating anything.
- Reject duplicate subsystem names, and do it before any allocation or output.
- Break block-size ties by input file order using a stable sort, and log the tie
  to stderr.
- Use `ipaddress` for all address math.
- Keep parse/size/allocate as pure functions, with I/O confined to `main`.
- Cite the input line number in every parse error.

### Ask first

- Adding any runtime dependency (the target is zero).
- Adding a CLI flag, or changing the output columns or their order.
- Changing the default `--base`.
- Changing the definition of leftover capacity, or the `+2` rule.
- Supporting `/31` point-to-point links.
- Adding a second allocation strategy (best-fit, gap-filling, fixed reservations).
- Anything that writes files other than `--output`.
- `git init` and the first commit.

### Never do

- Make network calls, or read the machine's own network configuration.
- Emit overlapping or misaligned blocks — this is the one class of bug that
  produces a plan that looks right and breaks in production.
- Accept a prefix length on `--base` and silently ignore it.
- Silently default a missing `span` column to 0.
- Allocate two blocks under the same subsystem name.
- Add a secondary sort key to break ties — input order is the tie-breaker, and a
  name-based fallback would silently reorder plans when the input is reordered.
- Write tie notes, warnings, or anything but the report itself to stdout.
- Round demands down, or shrink a block to fit remaining space.
- Add IPv6 handling to this tool.
- Reformat, restructure, or "clean up" code outside the task at hand.
- Guess at an ambiguous requirement and build past it — stop and ask.

---

## Open questions

1. **Misaligned base — reject or auto-adjust?** Spec'd as reject with the nearest
   valid addresses named. Auto-rounding up to the next aligned address would
   always succeed but silently move the plan off the address you asked for.
2. **Does the summary line matter to you?** Shown under the example table; easy
   to drop if it's noise.

### Resolved

- Span sits **outside** the `+2` — sizing is `hosts + span + 2`.
- Leftover capacity counts span as **used** — `usable − (hosts + span)`.
- Duplicate subsystem names are **rejected** (exit 2), checked before allocation.
- Block-size ties are broken by **input file order**, with a note logged to stderr.
- `--strict` removed; `-q` added to silence tie notes.
- `--base` is a bare address; Python; default base `192.168.0.0`.

---

## Next step

All sizing and ordering decisions are now settled. The two remaining open
questions are cosmetic, with defaults already chosen — the spec is buildable as
written without answering them.

On approval: task breakdown, then implementation. This is small enough that the
whole build is roughly five tasks — parse, size, sort/allocate, render, CLI —
each with the tests above as its acceptance criteria.
