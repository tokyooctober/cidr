"""Tests for cidrplan. See SPEC.md for the behaviour being verified."""

import io
import ipaddress
import json
import random
from itertools import pairwise
from pathlib import Path

import pytest

from cidrplan import (
    Allocation,
    LayoutError,
    ParseError,
    Subsystem,
    allocate,
    block_size,
    main,
    parse_input,
    prefix_length,
    render_csv,
    render_json,
    render_text,
    tie_groups,
)

DEFAULT_BASE = ipaddress.IPv4Address("192.168.0.0")


def subsystems(*rows: tuple[str, int]) -> list[Subsystem]:
    """Build subsystems directly, bypassing the parser."""
    return [Subsystem(name, hosts, i) for i, (name, hosts) in enumerate(rows, 1)]


def plan(*rows: tuple[str, int], base: str = "192.168.0.0") -> list[Allocation]:
    return allocate(subsystems(*rows), ipaddress.IPv4Address(base))


def parse(text: str) -> list[Subsystem]:
    """Parse a whole input document given as a single string."""
    return parse_input(text.splitlines())


class TestParseInputHappyPath:
    def test_parses_two_columns_into_subsystems(self) -> None:
        result = parse("Sales 300\nEngineering 120\n")

        assert result == [
            Subsystem(name="Sales", hosts=300, line=1),
            Subsystem(name="Engineering", hosts=120, line=2),
        ]

    def test_records_the_source_line_number_of_each_subsystem(self) -> None:
        result = parse("# header\n\nSales 300\n\nOps 25\n")

        assert [(s.name, s.line) for s in result] == [("Sales", 3), ("Ops", 5)]

    def test_skips_comment_lines_and_blank_lines(self) -> None:
        result = parse("# a comment\n\n   \nSales 300\n# another\n")

        assert [s.name for s in result] == ["Sales"]

    def test_accepts_tab_separated_fields(self) -> None:
        assert parse("Sales\t300\n") == [Subsystem("Sales", 300, 1)]

    def test_accepts_comma_separated_fields(self) -> None:
        assert parse("Sales,300\n") == [Subsystem("Sales", 300, 1)]

    def test_accepts_runs_of_spaces_as_a_single_separator(self) -> None:
        assert parse("Sales      300\n") == [Subsystem("Sales", 300, 1)]

    def test_name_may_contain_spaces_because_only_the_last_field_is_numeric(self) -> None:
        assert parse("HR Department 45\n") == [Subsystem("HR Department", 45, 1)]

    def test_strips_surrounding_whitespace_from_the_name(self) -> None:
        assert parse("   Sales    300   \n") == [Subsystem("Sales", 300, 1)]

    def test_empty_input_produces_no_subsystems(self) -> None:
        assert parse("") == []

    def test_input_of_only_comments_produces_no_subsystems(self) -> None:
        assert parse("# just a comment\n\n") == []


class TestParseInputRejectsBadInput:
    def test_rejects_a_line_with_only_a_name(self) -> None:
        with pytest.raises(ParseError) as exc:
            parse("Sales\n")

        assert "line 1" in str(exc.value)

    def test_rejects_hosts_of_zero(self) -> None:
        with pytest.raises(ParseError, match="hosts"):
            parse("Sales 0\n")

    def test_rejects_negative_hosts(self) -> None:
        with pytest.raises(ParseError, match="hosts"):
            parse("Sales -5\n")

    def test_rejects_non_numeric_hosts(self) -> None:
        with pytest.raises(ParseError, match="hosts"):
            parse("Sales abc\n")

    def test_rejects_fractional_hosts(self) -> None:
        with pytest.raises(ParseError, match="hosts"):
            parse("Sales 3.5\n")

    def test_error_cites_the_offending_line_number_not_the_subsystem_index(self) -> None:
        with pytest.raises(ParseError) as exc:
            parse("# comment\nSales 300\n\nOps 0\n")

        assert "line 4" in str(exc.value)


class TestParseInputRejectsDuplicateNames:
    def test_rejects_a_duplicate_subsystem_name(self) -> None:
        with pytest.raises(ParseError, match="duplicate"):
            parse("Sales 300\nSales 120\n")

    def test_duplicate_error_names_both_line_numbers(self) -> None:
        with pytest.raises(ParseError) as exc:
            parse("Sales 300\nOps 25\nSales 120\n")

        message = str(exc.value)
        assert "'Sales'" in message
        assert "line 3" in message
        assert "line 1" in message

    def test_rejects_a_duplicate_on_the_very_last_line(self) -> None:
        with pytest.raises(ParseError, match="duplicate"):
            parse("Sales 300\nOps 25\nGuest 10\nSales 1\n")

    def test_names_differing_only_in_surrounding_whitespace_are_duplicates(self) -> None:
        with pytest.raises(ParseError, match="duplicate"):
            parse("Sales 300\n   Sales    120\n")

    def test_names_differing_only_in_case_are_not_duplicates(self) -> None:
        result = parse("Sales 300\nsales 120\n")

        assert [s.name for s in result] == ["Sales", "sales"]


# (hosts, block size, prefix, usable, leftover)
# This is the boundary table from SPEC.md section 5.
SIZING_TABLE = [
    (1, 4, 30, 2, 1),
    (2, 4, 30, 2, 0),
    (3, 8, 29, 6, 3),
    (6, 8, 29, 6, 0),
    (7, 16, 28, 14, 7),
    (14, 16, 28, 14, 0),
    (30, 32, 27, 30, 0),
    (254, 256, 24, 254, 0),
    (255, 512, 23, 510, 255),
    (300, 512, 23, 510, 210),
    (510, 512, 23, 510, 0),
    (511, 1024, 22, 1022, 511),
]

SAMPLE_HOST_COUNTS = [1, 2, 3, 6, 7, 14, 30, 63, 64, 65, 253, 254, 255, 300, 1000]


class TestBlockSize:
    @pytest.mark.parametrize("hosts,expected_size,_prefix,_usable,_left", SIZING_TABLE)
    def test_block_size_matches_the_spec_boundary_table(
        self, hosts: int, expected_size: int, _prefix: int, _usable: int, _left: int
    ) -> None:
        assert block_size(hosts) == expected_size

    @pytest.mark.parametrize("hosts,_size,expected_prefix,_usable,_left", SIZING_TABLE)
    def test_prefix_length_matches_the_spec_boundary_table(
        self, hosts: int, _size: int, expected_prefix: int, _usable: int, _left: int
    ) -> None:
        assert prefix_length(block_size(hosts)) == expected_prefix

    @pytest.mark.parametrize("hosts,_size,_prefix,expected_usable,_left", SIZING_TABLE)
    def test_usable_capacity_is_block_size_minus_network_and_broadcast(
        self, hosts: int, _size: int, _prefix: int, expected_usable: int, _left: int
    ) -> None:
        assert block_size(hosts) - 2 == expected_usable

    @pytest.mark.parametrize("hosts,_size,_prefix,_usable,expected_leftover", SIZING_TABLE)
    def test_leftover_is_usable_capacity_minus_hosts(
        self, hosts: int, _size: int, _prefix: int, _usable: int, expected_leftover: int
    ) -> None:
        assert (block_size(hosts) - 2) - hosts == expected_leftover

    def test_an_exact_fit_does_not_jump_to_the_next_prefix(self) -> None:
        # 254 fits a /24 exactly; one more host forces a /23.
        assert block_size(254) == 256
        assert block_size(255) == 512

    @pytest.mark.parametrize("hosts", SAMPLE_HOST_COUNTS)
    def test_leftover_is_never_negative(self, hosts: int) -> None:
        assert block_size(hosts) - 2 >= hosts

    @pytest.mark.parametrize("hosts", SAMPLE_HOST_COUNTS)
    def test_block_size_is_always_a_power_of_two(self, hosts: int) -> None:
        size = block_size(hosts)

        assert size & (size - 1) == 0

    @pytest.mark.parametrize("hosts", SAMPLE_HOST_COUNTS)
    def test_block_is_minimal_because_halving_it_would_not_fit(self, hosts: int) -> None:
        assert (block_size(hosts) // 2) - 2 < hosts

    def test_smallest_block_is_a_slash_30_because_a_slash_31_has_no_usable_addresses(
        self,
    ) -> None:
        assert block_size(1) == 4
        assert prefix_length(4) == 30


EXAMPLE_ROWS = (
    ("Sales", 300),
    ("Engineering", 120),
    ("Warehouse", 60),
    ("Ops", 25),
    ("Guest", 10),
)


class TestAllocateWorkedExample:
    def test_reproduces_the_spec_worked_example(self) -> None:
        result = plan(*EXAMPLE_ROWS)

        assert [(a.subsystem.name, str(a.network), a.total_usable, a.leftover) for a in result] == [
            ("Sales", "192.168.0.0/23", 510, 210),
            ("Engineering", "192.168.2.0/25", 126, 6),
            ("Warehouse", "192.168.2.128/26", 62, 2),
            ("Ops", "192.168.2.192/27", 30, 5),
            ("Guest", "192.168.2.224/28", 14, 4),
        ]

    def test_usable_ranges_exclude_network_and_broadcast_addresses(self) -> None:
        result = plan(*EXAMPLE_ROWS)

        assert [(str(a.first_usable), str(a.last_usable)) for a in result] == [
            ("192.168.0.1", "192.168.1.254"),
            ("192.168.2.1", "192.168.2.126"),
            ("192.168.2.129", "192.168.2.190"),
            ("192.168.2.193", "192.168.2.222"),
            ("192.168.2.225", "192.168.2.238"),
        ]

    def test_a_slash_23_spans_two_third_octet_values(self) -> None:
        sales = plan(*EXAMPLE_ROWS)[0]

        assert str(sales.first_usable) == "192.168.0.1"
        assert str(sales.last_usable) == "192.168.1.254"


class TestAllocateOrdering:
    def test_sorts_by_block_size_descending_not_by_host_count(self) -> None:
        # 100 and 120 hosts both need a /25, so they tie and must keep input
        # order. Sorting by host count would put 120 first.
        result = plan(("Smaller", 100), ("Larger", 120))

        assert [a.subsystem.name for a in result] == ["Smaller", "Larger"]
        assert [str(a.network) for a in result] == ["192.168.0.0/25", "192.168.0.128/25"]

    def test_larger_blocks_come_first(self) -> None:
        result = plan(("Tiny", 5), ("Huge", 900), ("Middle", 100))

        assert [a.subsystem.name for a in result] == ["Huge", "Middle", "Tiny"]

    def test_ties_preserve_input_order(self) -> None:
        result = plan(("Zulu", 25), ("Alpha", 30), ("Mike", 20))

        assert [a.subsystem.name for a in result] == ["Zulu", "Alpha", "Mike"]

    def test_reversing_tied_input_reverses_output_proving_the_sort_is_stable(self) -> None:
        forward = plan(("Zulu", 25), ("Alpha", 30), ("Mike", 20))
        reverse = plan(("Mike", 20), ("Alpha", 30), ("Zulu", 25))

        assert [a.subsystem.name for a in forward] == ["Zulu", "Alpha", "Mike"]
        assert [a.subsystem.name for a in reverse] == ["Mike", "Alpha", "Zulu"]


class TestTieGroups:
    def test_reports_a_group_of_subsystems_sharing_a_block_size(self) -> None:
        result = plan(("Ops", 25), ("Lab", 30))

        assert tie_groups(result) == [(27, ["Ops", "Lab"])]

    def test_reports_no_groups_when_every_block_size_differs(self) -> None:
        assert tie_groups(plan(*EXAMPLE_ROWS)) == []

    def test_reports_each_tied_prefix_separately(self) -> None:
        result = plan(("A", 25), ("B", 30), ("C", 100), ("D", 120))

        assert tie_groups(result) == [(25, ["C", "D"]), (27, ["A", "B"])]


class TestAllocateBaseAlignment:
    def test_rejects_a_base_not_aligned_to_the_largest_block(self) -> None:
        with pytest.raises(LayoutError, match="aligned"):
            plan(*EXAMPLE_ROWS, base="192.168.1.0")

    def test_alignment_error_names_the_nearest_valid_addresses(self) -> None:
        with pytest.raises(LayoutError) as exc:
            plan(*EXAMPLE_ROWS, base="192.168.1.0")

        message = str(exc.value)
        assert "192.168.0.0" in message
        assert "192.168.2.0" in message

    def test_accepts_a_base_that_is_aligned(self) -> None:
        result = plan(*EXAMPLE_ROWS, base="192.168.2.0")

        assert str(result[0].network) == "192.168.2.0/23"

    def test_alignment_is_judged_against_the_largest_block_not_the_first_row(self) -> None:
        # Guest is listed first but Sales is allocated first, so the base must
        # satisfy the /23, not the /28.
        with pytest.raises(LayoutError, match="aligned"):
            plan(("Guest", 10), ("Sales", 300), base="192.168.1.0")


class TestAllocateOverflow:
    def test_rejects_a_plan_running_past_the_end_of_ipv4_space(self) -> None:
        # The plan needs 752 addresses; the last /23-aligned base leaves 512.
        with pytest.raises(LayoutError, match="255.255.255.255"):
            plan(*EXAMPLE_ROWS, base="255.255.254.0")

    def test_a_plan_ending_exactly_at_the_last_address_is_allowed(self) -> None:
        result = plan(("Edge", 250), base="255.255.255.0")

        assert str(result[0].network) == "255.255.255.0/24"

    def test_a_plan_filling_the_final_1024_addresses_exactly_is_allowed(self) -> None:
        # Off-by-one guard: this ends on 255.255.255.255 and must not be
        # mistaken for an overflow.
        result = plan(("Big", 300), ("Another", 300), base="255.255.252.0")

        assert str(result[-1].network.broadcast_address) == "255.255.255.255"


class TestAllocateEdgeCases:
    def test_empty_input_produces_no_allocations(self) -> None:
        assert allocate([], DEFAULT_BASE) == []

    def test_a_single_subsystem_is_allocated_at_the_base(self) -> None:
        result = plan(("Only", 10))

        assert str(result[0].network) == "192.168.0.0/28"

    def test_allocations_are_contiguous_with_no_gaps(self) -> None:
        result = plan(*EXAMPLE_ROWS)

        for previous, following in pairwise(result):
            assert (
                int(following.network.network_address)
                == int(previous.network.broadcast_address) + 1
            )


class TestAllocateProperties:
    """Randomised checks of the invariants that make a plan valid."""

    @staticmethod
    def random_rows(seed: int) -> list[tuple[str, int]]:
        rng = random.Random(seed)
        return [(f"sub{i}", rng.randint(1, 4000)) for i in range(rng.randint(1, 12))]

    @pytest.mark.parametrize("seed", range(60))
    def test_blocks_never_overlap(self, seed: int) -> None:
        result = plan(*self.random_rows(seed), base="10.0.0.0")

        for a, b in pairwise(result):
            assert int(a.network.broadcast_address) < int(b.network.network_address)

    @pytest.mark.parametrize("seed", range(60))
    def test_every_block_holds_at_least_its_host_count(self, seed: int) -> None:
        for a in plan(*self.random_rows(seed), base="10.0.0.0"):
            assert a.total_usable >= a.subsystem.hosts

    @pytest.mark.parametrize("seed", range(60))
    def test_every_block_is_aligned_to_its_own_size(self, seed: int) -> None:
        for a in plan(*self.random_rows(seed), base="10.0.0.0"):
            assert int(a.network.network_address) % a.network.num_addresses == 0

    @pytest.mark.parametrize("seed", range(60))
    def test_leftover_is_always_consistent_and_non_negative(self, seed: int) -> None:
        for a in plan(*self.random_rows(seed), base="10.0.0.0"):
            assert a.leftover == a.total_usable - a.subsystem.hosts
            assert a.leftover >= 0

    @pytest.mark.parametrize("seed", range(60))
    def test_plan_is_contiguous_so_no_address_space_is_wasted(self, seed: int) -> None:
        result = plan(*self.random_rows(seed), base="10.0.0.0")
        consumed = sum(a.network.num_addresses for a in result)
        first = int(result[0].network.network_address)
        last = int(result[-1].network.broadcast_address)

        assert last - first + 1 == consumed

    @pytest.mark.parametrize("seed", range(60))
    def test_no_block_could_be_halved_and_still_fit(self, seed: int) -> None:
        for a in plan(*self.random_rows(seed), base="10.0.0.0"):
            assert (a.network.num_addresses // 2) - 2 < a.subsystem.hosts


# The golden table from SPEC.md section 2.
GOLDEN_TEXT = """\
SUBSYSTEM    HOSTS  USABLE IP RANGE                CIDR (TOTAL HOSTS)           LEFTOVER CAPACITY
Sales          300  192.168.0.1 - 192.168.1.254    192.168.0.0/23 (510 hosts)                 210
Engineering    120  192.168.2.1 - 192.168.2.126    192.168.2.0/25 (126 hosts)                   6
Warehouse       60  192.168.2.129 - 192.168.2.190  192.168.2.128/26 (62 hosts)                  2
Ops             25  192.168.2.193 - 192.168.2.222  192.168.2.192/27 (30 hosts)                  5
Guest           10  192.168.2.225 - 192.168.2.238  192.168.2.224/28 (14 hosts)                  4

Allocated 752 addresses from 192.168.0.0 through 192.168.2.239.
Next free address: 192.168.2.240"""


class TestRenderText:
    def test_reproduces_the_spec_golden_table_byte_for_byte(self) -> None:
        assert render_text(plan(*EXAMPLE_ROWS), DEFAULT_BASE) == GOLDEN_TEXT

    def test_numeric_columns_are_right_aligned(self) -> None:
        rows = render_text(plan(*EXAMPLE_ROWS), DEFAULT_BASE).splitlines()
        header, first = rows[0], rows[1]

        assert header.index("HOSTS") + len("HOSTS") == first.index("300") + len("300")

    def test_no_line_carries_trailing_whitespace(self) -> None:
        for line in render_text(plan(*EXAMPLE_ROWS), DEFAULT_BASE).splitlines():
            assert line == line.rstrip()

    def test_columns_widen_to_fit_a_long_subsystem_name(self) -> None:
        output = render_text(plan(("A Very Long Subsystem Name", 10)), DEFAULT_BASE)

        assert "A Very Long Subsystem Name" in output
        assert output.splitlines()[0].startswith("SUBSYSTEM" + " " * 18)

    def test_empty_plan_renders_a_header_and_no_rows(self) -> None:
        output = render_text([], DEFAULT_BASE)

        assert output.splitlines()[0].startswith("SUBSYSTEM")
        assert "Allocated 0 addresses" in output


class TestRenderCsv:
    def test_emits_a_header_row_of_six_fields(self) -> None:
        first = render_csv(plan(*EXAMPLE_ROWS)).splitlines()[0]

        assert first == "subsystem,hosts,usable_range,cidr,total_hosts,leftover"

    def test_splits_the_cidr_and_its_capacity_into_separate_fields(self) -> None:
        rows = render_csv(plan(*EXAMPLE_ROWS)).splitlines()

        assert rows[1] == "Sales,300,192.168.0.1 - 192.168.1.254,192.168.0.0/23,510,210"

    def test_carries_no_summary_line(self) -> None:
        assert "Allocated" not in render_csv(plan(*EXAMPLE_ROWS))

    def test_quotes_a_name_containing_a_comma(self) -> None:
        output = render_csv(plan(("Sales, EMEA", 10)))

        assert '"Sales, EMEA"' in output


class TestRenderJson:
    def test_carries_the_base_the_allocations_and_a_summary(self) -> None:
        payload = json.loads(render_json(plan(*EXAMPLE_ROWS), DEFAULT_BASE))

        assert payload["base"] == "192.168.0.0"
        assert len(payload["allocations"]) == 5
        assert payload["summary"]["total_addresses"] == 752

    def test_each_allocation_carries_every_reported_field(self) -> None:
        payload = json.loads(render_json(plan(*EXAMPLE_ROWS), DEFAULT_BASE))

        assert payload["allocations"][0] == {
            "subsystem": "Sales",
            "hosts": 300,
            "cidr": "192.168.0.0/23",
            "first_usable": "192.168.0.1",
            "last_usable": "192.168.1.254",
            "total_hosts": 510,
            "leftover": 210,
        }

    def test_is_valid_json_for_an_empty_plan(self) -> None:
        payload = json.loads(render_json([], DEFAULT_BASE))

        assert payload["allocations"] == []


EXAMPLE_FILE = """\
# subsystem      hosts
Sales              300
Engineering        120
Warehouse           60
Ops                 25
Guest               10
"""

TIED_FILE = "Ops 25\nLab 30\n"


@pytest.fixture
def example(tmp_path: Path) -> str:
    path = tmp_path / "subsystems.txt"
    path.write_text(EXAMPLE_FILE, encoding="utf-8")
    return str(path)


@pytest.fixture
def tied(tmp_path: Path) -> str:
    path = tmp_path / "ties.txt"
    path.write_text(TIED_FILE, encoding="utf-8")
    return str(path)


class TestCliSuccess:
    def test_prints_the_golden_table_and_exits_zero(
        self, example: str, capsys: pytest.CaptureFixture[str]
    ) -> None:
        code = main([example])

        assert code == 0
        assert capsys.readouterr().out.rstrip("\n") == GOLDEN_TEXT

    def test_defaults_to_base_192_168_0_0(
        self, example: str, capsys: pytest.CaptureFixture[str]
    ) -> None:
        main([example])

        assert "192.168.0.0/23" in capsys.readouterr().out

    def test_honours_an_explicit_base(
        self, example: str, capsys: pytest.CaptureFixture[str]
    ) -> None:
        main([example, "--base", "10.0.0.0"])

        assert "10.0.0.0/23" in capsys.readouterr().out

    def test_reads_the_plan_from_stdin_when_given_a_dash(
        self, capsys: pytest.CaptureFixture[str], monkeypatch: pytest.MonkeyPatch
    ) -> None:
        monkeypatch.setattr("sys.stdin", io.StringIO(EXAMPLE_FILE))

        code = main(["-"])

        assert code == 0
        assert "192.168.0.0/23" in capsys.readouterr().out

    def test_csv_format_reaches_stdout(
        self, example: str, capsys: pytest.CaptureFixture[str]
    ) -> None:
        main([example, "--format", "csv"])

        assert capsys.readouterr().out.startswith("subsystem,hosts,usable_range")

    def test_json_format_reaches_stdout(
        self, example: str, capsys: pytest.CaptureFixture[str]
    ) -> None:
        main([example, "--format", "json"])

        assert json.loads(capsys.readouterr().out)["base"] == "192.168.0.0"

    def test_order_input_restores_the_original_file_order(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        path = tmp_path / "unsorted.txt"
        path.write_text("Guest 10\nSales 300\nOps 25\n", encoding="utf-8")

        main([str(path), "--format", "csv", "--order", "input"])

        names = [row.split(",")[0] for row in capsys.readouterr().out.splitlines()[1:]]
        assert names == ["Guest", "Sales", "Ops"]

    def test_order_input_does_not_change_the_blocks_that_were_assigned(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        path = tmp_path / "unsorted.txt"
        path.write_text("Guest 10\nSales 300\nOps 25\n", encoding="utf-8")

        main([str(path), "--format", "csv", "--order", "input"])

        rows = {
            row.split(",")[0]: row.split(",")[3] for row in capsys.readouterr().out.splitlines()[1:]
        }
        assert rows["Sales"] == "192.168.0.0/23"
        assert rows["Guest"] == "192.168.2.32/28"

    def test_output_flag_writes_to_a_file_and_leaves_stdout_empty(
        self, example: str, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        destination = tmp_path / "plan.txt"

        main([example, "--output", str(destination)])

        assert destination.read_text(encoding="utf-8").rstrip("\n") == GOLDEN_TEXT
        assert capsys.readouterr().out == ""


class TestCliTieNotes:
    def test_tie_note_goes_to_stderr_not_stdout(
        self, tied: str, capsys: pytest.CaptureFixture[str]
    ) -> None:
        main([tied])

        captured = capsys.readouterr()
        assert "tie" in captured.err
        assert "tie" not in captured.out

    def test_tie_note_names_every_member_of_the_group(
        self, tied: str, capsys: pytest.CaptureFixture[str]
    ) -> None:
        main([tied])

        error = capsys.readouterr().err
        assert "Ops" in error
        assert "Lab" in error

    def test_quiet_suppresses_the_tie_note(
        self, tied: str, capsys: pytest.CaptureFixture[str]
    ) -> None:
        main([tied, "-q"])

        assert capsys.readouterr().err == ""

    def test_a_tie_does_not_change_the_exit_code(self, tied: str) -> None:
        assert main([tied]) == 0

    def test_no_note_is_emitted_when_no_block_sizes_tie(
        self, example: str, capsys: pytest.CaptureFixture[str]
    ) -> None:
        main([example])

        assert capsys.readouterr().err == ""


class TestCliErrors:
    def test_duplicate_name_exits_two_and_prints_nothing_to_stdout(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        path = tmp_path / "dupes.txt"
        path.write_text("Sales 300\nOps 25\nSales 10\n", encoding="utf-8")

        code = main([str(path)])

        captured = capsys.readouterr()
        assert code == 2
        assert captured.out == ""
        assert "duplicate" in captured.err

    def test_bad_host_count_exits_two_citing_the_line(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        path = tmp_path / "bad.txt"
        path.write_text("Sales 300\nOps 0\n", encoding="utf-8")

        code = main([str(path)])

        assert code == 2
        assert "line 2" in capsys.readouterr().err

    def test_base_with_a_prefix_exits_two_and_explains_why(
        self, example: str, capsys: pytest.CaptureFixture[str]
    ) -> None:
        code = main([example, "--base", "192.168.0.0/24"])

        captured = capsys.readouterr()
        assert code == 2
        assert "bare address" in captured.err
        assert captured.out == ""

    def test_a_malformed_base_address_exits_two(
        self, example: str, capsys: pytest.CaptureFixture[str]
    ) -> None:
        assert main([example, "--base", "not-an-address"]) == 2
        assert capsys.readouterr().err != ""

    def test_a_missing_input_file_exits_two(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        code = main([str(tmp_path / "nope.txt")])

        assert code == 2
        assert capsys.readouterr().err != ""

    def test_misaligned_base_exits_one_and_prints_nothing_to_stdout(
        self, example: str, capsys: pytest.CaptureFixture[str]
    ) -> None:
        code = main([example, "--base", "192.168.1.0"])

        captured = capsys.readouterr()
        assert code == 1
        assert captured.out == ""
        assert "aligned" in captured.err

    def test_ipv4_overflow_exits_one(
        self, example: str, capsys: pytest.CaptureFixture[str]
    ) -> None:
        code = main([example, "--base", "255.255.254.0"])

        assert code == 1
        assert "255.255.255.255" in capsys.readouterr().err

    def test_an_unwritable_output_path_exits_two(
        self, example: str, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        unreachable = tmp_path / "no-such-directory" / "plan.txt"

        code = main([example, "--output", str(unreachable)])

        captured = capsys.readouterr()
        assert code == 2
        assert "cannot write" in captured.err
        assert captured.out == ""

    def test_an_empty_input_file_succeeds_with_an_empty_table(
        self, tmp_path: Path, capsys: pytest.CaptureFixture[str]
    ) -> None:
        path = tmp_path / "empty.txt"
        path.write_text("", encoding="utf-8")

        code = main([str(path)])

        assert code == 0
        assert "SUBSYSTEM" in capsys.readouterr().out
