# cidrplan

Assign non-overlapping IPv4 CIDR blocks to subsystems from a list of host counts
and growth allowances.

Python 3.11+, standard library only. No install step.

## Usage

```bash
python cidrplan.py examples/subsystems.txt
```

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

## Input

Three whitespace- or comma-separated columns. The last two fields are the host
count and the span, so subsystem names may contain spaces without quoting.

```
# subsystem      hosts  span
Sales              300     50
HR Department       45     10
Guest               10      0
```

Blank lines and `#` comments are ignored. Duplicate subsystem names are rejected.

## Options

| Option | Default | Effect |
|---|---|---|
| `--base ADDRESS` | `192.168.0.0` | Starting address. Bare — a prefix length is an error. |
| `--format text\|csv\|json` | `text` | Output format. |
| `--order size\|input` | `size` | Report order. `input` restores file order without changing the blocks assigned. |
| `--output FILE` | stdout | Write the report to a file. |
| `-q, --quiet` | off | Suppress tie notes on stderr. |

## How blocks are sized

A subsystem asking for `hosts` addresses with `span` growth room needs
`hosts + span` usable addresses. Every block also loses two addresses to its
network and broadcast addresses, so the block must hold `hosts + span + 2`,
rounded up to the next power of two.

```
300 hosts + 50 span  ->  352 addresses needed  ->  512  ->  /23  ->  510 usable
```

Leftover capacity is `usable - (hosts + span)`: span counts as claimed, not spare.

Blocks are allocated **largest first**. That ordering is what keeps every block on
its own power-of-two boundary, which is why the plan comes out contiguous with no
wasted alignment gaps. Subsystems whose blocks come out the same size keep their
input file order, and a note naming them is written to stderr.

## Exit codes

| Code | Meaning |
|---|---|
| `0` | Plan produced. |
| `1` | Plan cannot be laid out — base not aligned to the largest block, or the plan runs past `255.255.255.255`. |
| `2` | Usage or input error, including duplicate subsystem names. |

Errors and notes go to stderr; only the report goes to stdout, so `--format csv`
and `--format json` stay parseable when piped.

## Development

```bash
python -m pytest
```

Optional gates, if the tools are installed:

```bash
python -m pytest --cov=cidrplan --cov-report=term-missing
python -m mypy --strict cidrplan.py
python -m ruff check .
```

See [SPEC.md](SPEC.md) for the full specification.
