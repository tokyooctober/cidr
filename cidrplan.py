"""cidrplan — VLSM subnet allocator.

Reads a two-column plan (subsystem, hosts) and assigns each subsystem a
correctly-sized, non-overlapping CIDR block laid out contiguously from a base
address. See SPEC.md for the full specification.
"""

from __future__ import annotations

import argparse
import csv
import io
import ipaddress
import json
import sys
from collections.abc import Iterable, Sequence
from dataclasses import dataclass

DEFAULT_BASE = "192.168.0.0"

ADDRESS_BITS = 32
LAST_IPV4_ADDRESS = 2**ADDRESS_BITS - 1

# Every block loses two addresses to the network and broadcast addresses, so a
# block must be sized to hold the host count plus these two.
UNUSABLE_PER_BLOCK = 2


class ParseError(ValueError):
    """Raised when the input file cannot be read as a valid plan."""


class LayoutError(ValueError):
    """Raised when a valid plan cannot be laid out at the requested base."""


@dataclass(frozen=True)
class Subsystem:
    """One row of the input file."""

    name: str
    hosts: int
    line: int


@dataclass(frozen=True)
class Allocation:
    """A subsystem and the block assigned to it."""

    subsystem: Subsystem
    network: ipaddress.IPv4Network

    @property
    def total_usable(self) -> int:
        """Addresses available to hosts, excluding network and broadcast."""
        return self.network.num_addresses - UNUSABLE_PER_BLOCK

    @property
    def first_usable(self) -> ipaddress.IPv4Address:
        return self.network.network_address + 1

    @property
    def last_usable(self) -> ipaddress.IPv4Address:
        return self.network.broadcast_address - 1

    @property
    def leftover(self) -> int:
        """Capacity beyond the requested host count."""
        return self.total_usable - self.subsystem.hosts


def block_size(hosts: int) -> int:
    """Smallest power-of-two block holding `hosts` usable addresses.

    Guarantees usable capacity >= hosts, so leftover is never negative.
    """
    total_needed = hosts + UNUSABLE_PER_BLOCK
    size = 1
    while size < total_needed:
        size *= 2
    return size


def prefix_length(size: int) -> int:
    """CIDR prefix length for a power-of-two block size."""
    return ADDRESS_BITS - size.bit_length() + 1


def allocate(subsystems: Sequence[Subsystem], base: ipaddress.IPv4Address) -> list[Allocation]:
    """Assign each subsystem a block, laid out contiguously from base.

    Largest block first: this is what keeps every block on its own power-of-two
    boundary, so the plan has no alignment gaps. Ties keep input order via a
    stable sort — different host counts routinely share a block size (100 and
    120 both need a /25), so a secondary key would silently reorder them.
    """
    if not subsystems:
        return []

    ordered = sorted(subsystems, key=lambda s: block_size(s.hosts), reverse=True)

    largest = block_size(ordered[0].hosts)
    cursor = int(base)
    if cursor % largest != 0:
        below = (cursor // largest) * largest
        above = below + largest
        raise LayoutError(
            f"base {base} is not aligned to the largest block "
            f"(/{prefix_length(largest)}, {largest} addresses). "
            f"Nearest valid starting addresses: {ipaddress.IPv4Address(below)} "
            f"or {ipaddress.IPv4Address(above)}."
        )

    total = sum(block_size(s.hosts) for s in ordered)
    if cursor + total - 1 > LAST_IPV4_ADDRESS:
        raise LayoutError(
            f"plan needs {total} addresses starting at {base}, which runs past "
            f"255.255.255.255. Choose a lower base address."
        )

    allocations: list[Allocation] = []
    for subsystem in ordered:
        size = block_size(subsystem.hosts)
        network = ipaddress.IPv4Network((cursor, prefix_length(size)))
        allocation = Allocation(subsystem=subsystem, network=network)
        # A negative leftover would mean the block is smaller than the host
        # count it was sized for, which is a bug rather than a user error.
        assert allocation.leftover >= 0, f"negative leftover for {subsystem.name}"
        allocations.append(allocation)
        cursor += size

    return allocations


def tie_groups(allocations: Sequence[Allocation]) -> list[tuple[int, list[str]]]:
    """Prefix lengths shared by more than one subsystem, longest block first."""
    by_prefix: dict[int, list[str]] = {}
    for allocation in allocations:
        by_prefix.setdefault(allocation.network.prefixlen, []).append(allocation.subsystem.name)
    return [(prefix, names) for prefix, names in sorted(by_prefix.items()) if len(names) > 1]


TEXT_HEADERS = (
    "SUBSYSTEM",
    "HOSTS",
    "USABLE IP RANGE",
    "CIDR (TOTAL HOSTS)",
    "LEFTOVER CAPACITY",
)
RIGHT_ALIGNED_COLUMNS = frozenset({1, 4})


def _text_row(allocation: Allocation) -> tuple[str, ...]:
    return (
        allocation.subsystem.name,
        str(allocation.subsystem.hosts),
        f"{allocation.first_usable} - {allocation.last_usable}",
        f"{allocation.network} ({allocation.total_usable} hosts)",
        str(allocation.leftover),
    )


def _summary_lines(allocations: Sequence[Allocation], base: ipaddress.IPv4Address) -> list[str]:
    consumed = sum(a.network.num_addresses for a in allocations)
    end = int(base) + consumed
    return [
        f"Allocated {consumed} addresses from {base} through {ipaddress.IPv4Address(end - 1)}."
        if consumed
        else f"Allocated 0 addresses from {base}.",
        f"Next free address: {ipaddress.IPv4Address(end)}",
    ]


def render_text(allocations: Sequence[Allocation], base: ipaddress.IPv4Address) -> str:
    """Render the aligned report table plus its summary."""
    rows = [_text_row(a) for a in allocations]
    widths = [
        max([len(TEXT_HEADERS[i])] + [len(row[i]) for row in rows])
        for i in range(len(TEXT_HEADERS))
    ]

    def line(cells: Sequence[str]) -> str:
        rendered = (
            f"{cells[i]:>{widths[i]}}" if i in RIGHT_ALIGNED_COLUMNS else f"{cells[i]:<{widths[i]}}"
            for i in range(len(TEXT_HEADERS))
        )
        return "  ".join(rendered).rstrip()

    body = [line(TEXT_HEADERS)] + [line(row) for row in rows]
    return "\n".join(body + [""] + _summary_lines(allocations, base))


def render_csv(allocations: Sequence[Allocation]) -> str:
    """Render the report as CSV, with the CIDR and its capacity split apart."""
    buffer = io.StringIO()
    writer = csv.writer(buffer, lineterminator="\n")
    writer.writerow(["subsystem", "hosts", "usable_range", "cidr", "total_hosts", "leftover"])
    for a in allocations:
        writer.writerow(
            [
                a.subsystem.name,
                a.subsystem.hosts,
                f"{a.first_usable} - {a.last_usable}",
                str(a.network),
                a.total_usable,
                a.leftover,
            ]
        )
    return buffer.getvalue().rstrip("\n")


def render_json(allocations: Sequence[Allocation], base: ipaddress.IPv4Address) -> str:
    """Render the report as JSON."""
    consumed = sum(a.network.num_addresses for a in allocations)
    payload = {
        "base": str(base),
        "allocations": [
            {
                "subsystem": a.subsystem.name,
                "hosts": a.subsystem.hosts,
                "cidr": str(a.network),
                "first_usable": str(a.first_usable),
                "last_usable": str(a.last_usable),
                "total_hosts": a.total_usable,
                "leftover": a.leftover,
            }
            for a in allocations
        ],
        "summary": {
            "total_addresses": consumed,
            "next_free": str(ipaddress.IPv4Address(int(base) + consumed)),
        },
    }
    return json.dumps(payload, indent=2)


def _parse_count(raw: str, field: str, line_number: int) -> int:
    """Parse a field that must be a whole number, or raise ParseError."""
    try:
        return int(raw)
    except ValueError:
        raise ParseError(f"line {line_number}: {field} must be an integer, got {raw!r}") from None


def parse_input(lines: Iterable[str]) -> list[Subsystem]:
    """Parse three-column plan text into subsystems.

    The last field of a line is the host count; everything before it is the
    name, so names may contain spaces without needing quoting.
    """
    subsystems: list[Subsystem] = []
    first_seen: dict[str, int] = {}

    for line_number, raw_line in enumerate(lines, start=1):
        stripped = raw_line.strip()
        if not stripped or stripped.startswith("#"):
            continue

        fields = stripped.replace(",", " ").split()
        if len(fields) < 2:
            raise ParseError(
                f"line {line_number}: expected 2 fields (name, hosts), "
                f"got {len(fields)}: {stripped!r}"
            )

        name = " ".join(fields[:-1])
        hosts = _parse_count(fields[-1], "hosts", line_number)

        if hosts < 1:
            raise ParseError(f"line {line_number}: hosts must be a positive integer, got {hosts}")

        # Checked across the whole file before any allocation happens, so a
        # duplicate on the last line fails before a partial plan is printed.
        if name in first_seen:
            raise ParseError(
                f"duplicate subsystem name {name!r} on line {line_number} "
                f"(first seen on line {first_seen[name]})"
            )
        first_seen[name] = line_number

        subsystems.append(Subsystem(name=name, hosts=hosts, line=line_number))

    return subsystems


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        prog="cidrplan",
        description="Assign CIDR blocks to subsystems from a list of host counts.",
    )
    parser.add_argument("input", help="two-column plan file, or - for stdin")
    parser.add_argument(
        "--base",
        default=DEFAULT_BASE,
        help=f"starting address, bare with no prefix (default: {DEFAULT_BASE})",
    )
    parser.add_argument("--format", choices=("text", "csv", "json"), default="text")
    parser.add_argument("--order", choices=("size", "input"), default="size")
    parser.add_argument("--output", help="write the report here instead of stdout")
    parser.add_argument("-q", "--quiet", action="store_true", help="suppress tie notes on stderr")
    return parser


def _parse_base(raw: str) -> ipaddress.IPv4Address:
    """Parse the --base argument, rejecting anything carrying a prefix."""
    if "/" in raw:
        raise ParseError(
            "--base takes a bare address, not a network. Prefix lengths are "
            f"computed from the host counts. Use --base {raw.split('/')[0]}."
        )
    try:
        return ipaddress.IPv4Address(raw)
    except ValueError:
        raise ParseError(f"--base is not a valid IPv4 address: {raw!r}") from None


def _read_lines(source: str) -> list[str]:
    if source == "-":
        return sys.stdin.read().splitlines()
    try:
        with open(source, encoding="utf-8") as handle:
            return handle.read().splitlines()
    except OSError as error:
        raise ParseError(f"cannot read {source}: {error.strerror}") from None


def main(argv: Sequence[str] | None = None) -> int:
    """Entry point. Returns the process exit code."""
    args = _build_parser().parse_args(argv)

    try:
        base = _parse_base(args.base)
        subsystems = parse_input(_read_lines(args.input))
    except ParseError as error:
        print(f"error: {error}", file=sys.stderr)
        return 2

    try:
        allocations = allocate(subsystems, base)
    except LayoutError as error:
        print(f"error: {error}", file=sys.stderr)
        return 1

    if not args.quiet:
        for prefix, names in tie_groups(allocations):
            print(
                f"note: {len(names)} subsystems tie at /{prefix} "
                f"({', '.join(names)}); allocated in input file order.",
                file=sys.stderr,
            )

    if args.order == "input":
        allocations = sorted(allocations, key=lambda a: a.subsystem.line)

    if args.format == "csv":
        report = render_csv(allocations)
    elif args.format == "json":
        report = render_json(allocations, base)
    else:
        report = render_text(allocations, base)

    if args.output:
        try:
            with open(args.output, "w", encoding="utf-8") as handle:
                handle.write(report + "\n")
        except OSError as error:
            print(f"error: cannot write {args.output}: {error.strerror}", file=sys.stderr)
            return 2
    else:
        print(report)

    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
