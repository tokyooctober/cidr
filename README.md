# cidrplan

Assign non-overlapping IPv4 CIDR blocks to subsystems from a list of host counts.

Python 3.11+, standard library only. No install step.

## Usage

```bash
python cidrplan.py examples/subsystems.txt
```

```
SUBSYSTEM    HOSTS  USABLE IP RANGE                CIDR (TOTAL HOSTS)           LEFTOVER CAPACITY
Sales          300  192.168.0.1 - 192.168.1.254    192.168.0.0/23 (510 hosts)                 210
Engineering    120  192.168.2.1 - 192.168.2.126    192.168.2.0/25 (126 hosts)                   6
Warehouse       60  192.168.2.129 - 192.168.2.190  192.168.2.128/26 (62 hosts)                  2
Ops             25  192.168.2.193 - 192.168.2.222  192.168.2.192/27 (30 hosts)                  5
Guest           10  192.168.2.225 - 192.168.2.238  192.168.2.224/28 (14 hosts)                  4

Allocated 752 addresses from 192.168.0.0 through 192.168.2.239.
Next free address: 192.168.2.240
```

## Input

Two whitespace- or comma-separated columns. The last field is the host count, so
subsystem names may contain spaces without quoting.

```
# subsystem      hosts
Sales              300
HR Department       45
Guest               10
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

A subsystem asking for `hosts` addresses also needs two more for its network and
broadcast addresses, which are not assignable. The block must hold `hosts + 2`,
rounded up to the next power of two.

```
300 hosts  ->  302 addresses needed  ->  512  ->  /23  ->  510 usable
```

Leftover capacity is `usable - hosts`.

Blocks are allocated **largest first**. That ordering is what keeps every block on
its own power-of-two boundary, which is why the plan comes out contiguous with no
wasted alignment gaps. Different host counts often share a block size — 100 and
120 hosts both need a `/25` — and those keep their input file order, with a note
naming them written to stderr.

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
