# SPEC: `cidrplan` — VLSM subnet allocator

**Status:** Implemented
**Updated:** 2026-08-07
**Repo:** `C:\src\cidr`

---

## 1. Objective

### Problem

Given a list of subsystems and how many hosts each one needs, produce a
non-overlapping IPv4 addressing plan: one correctly-sized CIDR block per
subsystem, laid out contiguously from a starting address. Done by hand this is
slow and error-prone — off-by-two mistakes on usable counts, misaligned blocks,
and silent overlaps are all easy to make and hard to spot.

### What we're building

A small command-line program that reads a two-column text file (subsystem name,
host count) and emits an allocation table: subsystem, hosts, network address,
usable IP range, broadcast address, assigned CIDR with its total capacity, and
leftover capacity.

### Algorithm (the core of the spec)

1. **Read** the input file into `(name, hosts)` pairs.
2. **Size each block.** A subsystem needing `hosts` usable addresses also needs
   two more for the network and broadcast addresses, which are not assignable:

   ```
   total_needed = hosts + 2
   ```

   Round `total_needed` up to the next power of two to get block size `S`; the
   prefix length is `32 - log2(S)`. This guarantees `usable ≥ hosts` for every
   input, so leftover capacity can never go negative.
3. **Sort descending by block size `S`** (largest block first). This is not
   cosmetic: allocating largest-first is what guarantees every block lands on its
   natural power-of-two boundary, so the plan is contiguous with zero alignment
   gaps.

   **Ties are broken by input file order** — a stable sort, no secondary key.
   Different host counts routinely share a block size (100 and 120 hosts both
   need a `/25`), so sorting by host count instead of block size would silently
   reorder tied subsystems. When two or more subsystems tie, a note is logged to
   stderr naming them, so the ordering is visible rather than arbitrary.
4. **Allocate sequentially** starting at the base address, advancing the cursor
   by `S` after each block.
5. **Report** each subsystem with its network address, usable IP range (first
   usable → last usable), broadcast address, CIDR block with that block's total
   usable capacity, and leftover capacity = `total_usable - hosts`.

Worked example, verified: **300 hosts → 302 needed → 512 → `/23`**, giving 510
usable and 210 leftover.

### Target user

Whoever is planning the address space — run locally, one file in, one table out.
Single operator, no server, no persistence.

### Success criteria

- Blocks never overlap, and every block is aligned to its own size.
- Every subsystem's usable capacity is `≥ hosts`.
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
- Per-subsystem growth allowances. A `span` column was specified and built, then
  removed — sizing is driven by host count alone. Growth headroom, if wanted, is
  expressed by inflating the host count.

---

## Design decisions

1. **`--base` is a bare address with no prefix length.** `192.168.0.0`, not
   `192.168.0.0/24`. There is therefore **no bounding network and no overall
   capacity limit** — the plan extends upward from the base address as far as the
   demands require. Supplying a prefix is a usage error.
2. **Default base is `192.168.0.0`.**
3. **Leftover capacity = total usable − hosts.** For Sales: 510 − 300 = 210.
4. **The base address must be aligned to the largest block.** Since the largest
   block is allocated first, an unaligned base makes that first block invalid.
   `192.168.0.0` with a `/23` first block is fine; `192.168.1.0` is not. Rejected
   with the nearest valid addresses named.
5. **Duplicate subsystem names are rejected**, checked across the whole file
   before any allocation.
6. **Language: Python 3.11+, standard library only.** The built-in `ipaddress`
   module handles all address arithmetic, so this is a single file with no build
   step and no runtime dependencies.
7. **Minimum block is `/30`.** Under the network+broadcast rule, a `/31` has zero
   usable addresses. So 1 or 2 hosts round up to `/30` (4 addresses, 2 usable).
   RFC 3021 `/31` point-to-point links are not supported.

---

## 2. Commands

### Usage

```
python cidrplan.py <input-file> [--base ADDRESS] [--format text|csv|json]
                                [--order size|input] [--output FILE] [-q]
```

| Option | Default | Effect |
|---|---|---|
| `<input-file>` | *(required)* | Two-column input. `-` reads stdin. |
| `--base ADDRESS` | `192.168.0.0` | Starting address. A bare IPv4 address — **a prefix length is a usage error**. |
| `--format` | `text` | `text` = aligned table; `csv` and `json` for downstream tooling. |
| `--order` | `size` | `size` = allocation order (largest block first). `input` = original file order. |
| `--output FILE` | stdout | Write the report to a file instead. |
| `-q, --quiet` | off | Suppress the tie notes on stderr. Does not suppress errors. |

### Input format

Two columns, one subsystem per line:

```
# subsystem      hosts
Sales              300
Engineering        120
Warehouse           60
Ops                 25
Guest               10
```

Parsing rules:

- **The last whitespace- or comma-separated field is the host count; everything
  before it is the name.** This lets names contain spaces (`HR Department  45`)
  without quoting or a configured delimiter.
- Tab, comma, or runs of spaces all work as separators.
- Lines starting with `#` and blank lines are skipped.
- `hosts` must be a positive integer. Zero, negatives, non-numerics, and lines
  with the wrong field count are errors citing the offending line number.
- **Duplicate subsystem names are rejected.** Comparison is exact and
  case-sensitive, after trimming surrounding whitespace. The whole file is
  checked before any allocation happens, so a duplicate on the last line fails
  before a partial plan is printed.

### Output

**Seven columns.** The first two are copied from the input file; the last five
are computed.

| # | Column | Source | Content |
|---|---|---|---|
| 1 | Subsystem | input | Subsystem name, verbatim. |
| 2 | Hosts | input | Requested host count. |
| 3 | Network | computed | First address of the block. Not assignable. |
| 4 | Usable IP Range | computed | First usable → last usable address. |
| 5 | Broadcast | computed | Last address of the block. Not assignable. |
| 6 | CIDR (Total Hosts) | computed | Assigned block and its total usable capacity. |
| 7 | Leftover Capacity | computed | Column 6's capacity minus column 2. |

Network and broadcast **bracket** the usable range rather than sitting at the end
of the row, so each block reads left to right: what it starts on, what is
assignable inside it, what it ends on. The two unassignable addresses are then
visually adjacent to the range they are excluded from, which is the whole reason
the `+2` exists.

Numeric columns (2 and 7) are right-aligned; text columns are left-aligned. Every
column is padded to the width of its widest cell, header included. Trailing
whitespace is stripped from each line.

Verified output for the input above, with the default base:

```
SUBSYSTEM    HOSTS  NETWORK        USABLE IP RANGE                BROADCAST      CIDR (TOTAL HOSTS)           LEFTOVER CAPACITY
Sales          300  192.168.0.0    192.168.0.1 - 192.168.1.254    192.168.1.255  192.168.0.0/23 (510 hosts)                 210
Engineering    120  192.168.2.0    192.168.2.1 - 192.168.2.126    192.168.2.127  192.168.2.0/25 (126 hosts)                   6
Warehouse       60  192.168.2.128  192.168.2.129 - 192.168.2.190  192.168.2.191  192.168.2.128/26 (62 hosts)                  2
Ops             25  192.168.2.192  192.168.2.193 - 192.168.2.222  192.168.2.223  192.168.2.192/27 (30 hosts)                  5
Guest           10  192.168.2.224  192.168.2.225 - 192.168.2.238  192.168.2.239  192.168.2.224/28 (14 hosts)                  4

Allocated 752 addresses from 192.168.0.0 through 192.168.2.239.
Next free address: 192.168.2.240
```

**This table is the primary acceptance test.** It must reproduce byte for byte.

Note how Sales spans two third-octet values — `192.168.0.1` through
`192.168.1.254` — because a `/23` is two `/24`s wide. Its broadcast address is
`192.168.1.255`, so the next subsystem starts at `192.168.2.0`, not
`192.168.1.0`.

The network address is also derivable from the CIDR column (`192.168.0.0/23`
begins at `192.168.0.0`), so column 3 is redundant with column 6 for a reader who
does the arithmetic. It is carried explicitly because a plan is usually read by
someone transcribing addresses into device configuration, where re-deriving it is
an error opportunity.

`csv` emits eight fields — the seven columns with the CIDR and its capacity split
apart (`subsystem,hosts,network,usable_range,broadcast,cidr,total_hosts,leftover`)
— plus a header row and no summary. `json` emits an object with `base`, an
`allocations` array, and a `summary` object; each allocation carries
`network_address` and `broadcast_address` alongside `first_usable` and
`last_usable`.

### Tie notes

When subsystems produce equal block sizes, allocation follows input file order and
a note goes to **stderr** — never stdout, so `csv` and `json` output stays clean
when piped:

```
note: 2 subsystems tie at /25 (Engineering, Support); allocated in input file order.
```

One note per tie group. Suppressed by `-q`. Notes never change the exit code.

### Errors

**Duplicate subsystem name** — rejected before anything is allocated:

```
error: duplicate subsystem name 'Sales' on line 7 (first seen on line 2)
```

**Misaligned base** — the base must sit on a boundary of the largest block:

```
error: base 192.168.1.0 is not aligned to the largest block (/23, 512 addresses).
       Nearest valid starting addresses: 192.168.0.0 or 192.168.2.0.
```

**Base carries a prefix:**

```
error: --base takes a bare address, not a network. Prefix lengths are computed
       from the host counts. Use --base 192.168.0.0.
```

**Plan runs past the end of IPv4 space:**

```
error: plan needs 752 addresses starting at 255.255.254.0, which runs past
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
```

---

## 3. Project structure

Deliberately flat. This is a few hundred lines of logic; a package hierarchy
would be more structure than the problem has.

```
cidr/
├── cidrplan.py           # Python implementation: parse → size → sort → allocate → render
├── test_cidrplan.py      # pytest suite
├── go/                   # Go implementation, self-contained module
│   ├── cidrplan.go
│   ├── cidrplan_test.go
│   └── go.mod
├── examples/
│   └── subsystems.txt    # the worked example from §2
├── SPEC.md
├── README.md
└── pyproject.toml        # ruff + mypy + pytest config; no runtime deps
```

There are **two implementations of the same specification**. Python at the
repository root is the reference; Go under `go/` is a port. Both are complete,
independently tested, and produce the same report — see §7.

Internal organization of `cidrplan.py`, in order:

1. `Subsystem` and `Allocation` dataclasses — the two data shapes.
2. `block_size(hosts)` — the `+2` → next-power-of-two math.
3. `allocate(subsystems, base)` — sort by block size, walk, assign. Validates
   base alignment and IPv4 overflow.
4. `tie_groups(allocations)` — prefix lengths shared by more than one subsystem.
5. `render_text` / `render_csv` / `render_json` — the only functions that format.
6. `parse_input(lines)` — pure, no file I/O.
7. `main(argv)` — argument parsing, file I/O, exit codes.

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
- **Naming says which integer it is:** `hosts`, `total_addresses`,
  `usable_addresses`, `prefix_length`, `leftover`. Never a bare `n`, `size`, or
  `count` in the allocation logic — `hosts` and `usable_addresses` differ by
  exactly the padding this program exists to compute, and conflating them is the
  likeliest bug.
- **Errors:** raise `ParseError` with the line number and offending text for bad
  input; `LayoutError` for alignment and overflow failures. `main` is the only
  place that catches and converts to an exit code. No bare `except:`.
- **Comments explain why.** Three places need one: the `+2`, the sort-by-block-
  size (not by host count), and the base-alignment check. All three look like
  candidates for "simplification" by someone who doesn't know why they're there.
- **No premature abstraction.** No allocation-strategy classes, no plugin registry
  for output formats.
- **Deterministic output.** Same input, same base → byte-identical output, every
  run.

---

## 5. Testing strategy

`pytest`, table-driven where the cases are enumerable.

### Unit tests — `block_size`

The sizing function is where an off-by-one does the most damage.

| Hosts | Needed (+2) | Block | Prefix | Usable | Leftover |
|---|---|---|---|---|---|
| 1 | 3 | 4 | /30 | 2 | 1 |
| 2 | 4 | 4 | /30 | 2 | 0 |
| 3 | 5 | 8 | /29 | 6 | 3 |
| 6 | 8 | 8 | /29 | 6 | 0 |
| 7 | 9 | 16 | /28 | 14 | 7 |
| 14 | 16 | 16 | /28 | 14 | 0 |
| 30 | 32 | 32 | /27 | 30 | 0 |
| 254 | 256 | 256 | /24 | 254 | 0 |
| 255 | 257 | 512 | /23 | 510 | 255 |
| **300** | **302** | **512** | **/23** | **510** | **210** |
| 510 | 512 | 512 | /23 | 510 | 0 |
| 511 | 513 | 1024 | /22 | 1022 | 511 |

The exact-fit rows (2, 6, 14, 30, 254, 510) are the critical ones: a `>=` vs `>`
error makes them jump a whole prefix and silently double the plan's size.

Leftover is never negative — a negative would mean the block is too small for the
host count, which is a bug. Assert it on every allocation, not just in tests.

### Sorting tests

- **Sort key is block size, not host count.** 100 hosts and 120 hosts both need a
  `/25`. Listed in that order, they must be allocated in that order — sorting by
  host count would put 120 first.
- **Tie stability:** three subsystems producing the same block size, listed in a
  non-alphabetical input order, must be allocated in that input order. Reversing
  their order in the input must reverse their order in the output — this is what
  proves the sort is stable rather than incidentally matching.
- **Tie note:** the stderr note names every member of the tie group, appears once
  per group, is absent under `-q`, and never appears on stdout.

### Property tests

Over randomly generated host lists:

- **No overlap:** every pair of allocated blocks is disjoint.
- **Sufficiency:** every allocation's usable capacity ≥ hosts.
- **Alignment:** every block's network address is a multiple of its block size.
- **Contiguity:** with largest-first ordering, zero gaps between consecutive
  blocks.
- **Minimality:** no allocation could use a longer prefix and still fit.
- **Leftover consistency:** `leftover == usable - hosts`, always ≥ 0.

### Golden test

The §2 example table, compared byte for byte. Covers parsing, sizing, sorting,
allocation, alignment, column padding, and rendering in one shot — including the
octet-crossing `/23`.

### Network and broadcast column tests

- Both addresses match the assigned block for every row.
- `first_usable == network_address + 1` and `last_usable == broadcast_address - 1`
  for every allocation — this is the `+2` rule stated as an invariant rather than
  a computation.
- The header places `NETWORK` before `USABLE IP RANGE` and `BROADCAST` after it.

### Error-path tests

- Duplicate subsystem names → exit 2, message names the subsystem and both line
  numbers, and **nothing is printed to stdout**.
- Duplicate on the last line of the file → still rejected before any output.
- Names differing only in case (`Sales` / `sales`) → both allowed.
- Names differing only in surrounding whitespace → treated as duplicates.
- Misaligned base → exit 1, message names both nearest valid addresses.
- `--base` given with a prefix → exit 2, message explains that prefixes are
  computed.
- Plan runs past `255.255.255.255` → exit 1. A plan ending exactly on
  `255.255.255.255` is valid and must not be mistaken for an overflow.
- One-column line (missing hosts) → exit 2, correct line number.
- `hosts` of `0`, `-5`, `abc`, or `3.5` → exit 2, correct line number.
- Empty input → empty table, exit 0, no crash.
- Single subsystem → works.
- Names containing spaces → parsed correctly.

### Gates

- **100% line coverage on `block_size` and `allocate`.** They are small, pure, and
  carry all the risk.
- ≥90% overall.
- `mypy --strict` and `ruff check` clean.
- No test touches the network or writes outside `tmp_path`.

---

## 6. Boundaries

### Always do

- Compute usable addresses as `block_size - 2`, never as `block_size`.
- Size on `hosts + 2`.
- Sort by computed block size descending — never by host count.
- Validate that the base address is aligned to the largest block before
  allocating anything.
- Reject duplicate subsystem names, and do it before any allocation or output.
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
- Pushing to a remote.

### Never do

- Make network calls, or read the machine's own network configuration.
- Emit overlapping or misaligned blocks — this is the one class of bug that
  produces a plan that looks right and breaks in production.
- Accept a prefix length on `--base` and silently ignore it.
- Allocate two blocks under the same subsystem name.
- Add a secondary sort key to break ties — input order is the tie-breaker, and a
  name-based fallback would silently reorder plans when the input is reordered.
- Write tie notes, warnings, or anything but the report itself to stdout.
- Round host counts down, or shrink a block to fit remaining space.
- Add IPv6 handling to this tool.
- Guess at an ambiguous requirement and build past it — stop and ask.

---

## 7. The Go implementation

A second implementation of this same specification lives in `go/`, as a
self-contained module (`github.com/tokyooctober/cidr/go`). It exists so the plan
can be produced from a single static binary with no interpreter present.

### Rules

- **Python is the reference implementation.** Where the two disagree on anything
  other than the documented differences below, the Python behaviour is correct
  and the Go side is the bug.
- **Behaviour changes land in both, or in neither.** A change to sizing,
  ordering, output columns, or exit codes that lands in only one implementation
  is a defect, not a partial delivery.
- **Standard library only**, as with Python. `go.mod` has no `require` block and
  should stay that way.
- **No shared build.** The Go module is independent; the repository root is not a
  Go module and `go build ./go` from the root will fail. Build from inside `go/`.

### Commands

```
cd go
go build -o cidrplan .    # build the binary
go test ./...             # tests
go test -cover ./...      # coverage
go vet ./...              # vet
gofmt -l .                # format check (must print nothing)
```

### Parity

Verified by diffing all three output formats against the Python implementation
for the §2 worked example. The Go test suite carries the same golden table, the
same `block_size` boundary table, and the same allocation invariants.

### Documented differences

These two are real and intentional. Anything else is a bug.

1. **Line endings.** Python's `print` translates `\n` to `\r\n` on Windows; Go
   writes `\n` on every platform. The reports are otherwise byte-identical. Diff
   the two implementations with line endings normalised, not raw.
2. **Flag position.** Go's `flag` package stops parsing at the first non-flag
   argument, so flags must precede the input file:

   ```
   cidrplan -format csv plan.txt     # works
   cidrplan plan.txt -format csv     # flag silently ignored
   ```

   Python's `argparse` accepts either order. Rather than reimplementing argparse
   permutation, the Go version **detects** the second form and fails with exit 2
   explaining the rule — a silently ignored flag would otherwise produce a
   correct-looking report in the wrong format.

### Implementation notes

- Addresses are held as `uint64` internally and converted to `netip.Addr` only at
  the boundary, so the overflow check against `255.255.255.255` cannot itself
  wrap.
- Column padding counts **runes**, not bytes, so a non-ASCII subsystem name lines
  up the same way it does under Python.
- `json.Encoder` is configured with `SetEscapeHTML(false)` and two-space indent to
  match `json.dumps`, which escapes nothing and appends no trailing newline.
- The negative-leftover check panics rather than returning an error. It guards an
  invariant that only a code change can break, so it is not a user-facing error.

---

## Open questions

1. **Misaligned base — reject or auto-adjust?** Currently rejects with the nearest
   valid addresses named. Auto-rounding up to the next aligned address would
   always succeed but silently move the plan off the address you asked for.
2. **Does the summary line matter?** Shown under the example table; easy to drop
   if it's noise.
3. **Should the two implementations share golden fixtures?** Each currently
   carries its own copy of the golden table, so a change to output format means
   editing both. A shared `testdata/` file would remove that duplication at the
   cost of a file-read in tests that are otherwise pure.
