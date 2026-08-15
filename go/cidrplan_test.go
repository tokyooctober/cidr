package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/bits"
	"math/rand"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

var defaultBaseAddr = netip.MustParseAddr("192.168.0.0")

func subsystems(rows ...[2]any) []Subsystem {
	out := make([]Subsystem, 0, len(rows))
	for i, row := range rows {
		out = append(out, Subsystem{Name: row[0].(string), Hosts: row[1].(int), Line: i + 1})
	}
	return out
}

func planAt(t *testing.T, base string, rows ...[2]any) []Allocation {
	t.Helper()
	allocations, err := allocate(subsystems(rows...), netip.MustParseAddr(base))
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	return allocations
}

func plan(t *testing.T, rows ...[2]any) []Allocation {
	t.Helper()
	return planAt(t, "192.168.0.0", rows...)
}

var exampleRows = [][2]any{
	{"Sales", 300},
	{"Engineering", 120},
	{"Warehouse", 60},
	{"Ops", 25},
	{"Guest", 10},
}

const exampleFile = `# subsystem      hosts
Sales              300
Engineering        120
Warehouse           60
Ops                 25
Guest               10
`

// The golden table from SPEC.md section 2.
const goldenText = `SUBSYSTEM    HOSTS  NETWORK        USABLE IP RANGE                BROADCAST      CIDR (TOTAL HOSTS)           LEFTOVER CAPACITY
Sales          300  192.168.0.0    192.168.0.1 - 192.168.1.254    192.168.1.255  192.168.0.0/23 (510 hosts)                 210
Engineering    120  192.168.2.0    192.168.2.1 - 192.168.2.126    192.168.2.127  192.168.2.0/25 (126 hosts)                   6
Warehouse       60  192.168.2.128  192.168.2.129 - 192.168.2.190  192.168.2.191  192.168.2.128/26 (62 hosts)                  2
Ops             25  192.168.2.192  192.168.2.193 - 192.168.2.222  192.168.2.223  192.168.2.192/27 (30 hosts)                  5
Guest           10  192.168.2.224  192.168.2.225 - 192.168.2.238  192.168.2.239  192.168.2.224/28 (14 hosts)                  4

Allocated 752 addresses from 192.168.0.0 through 192.168.2.239.
Next free address: 192.168.2.240`

// --- block sizing ---------------------------------------------------------

func TestBlockSizeMatchesTheSpecBoundaryTable(t *testing.T) {
	// The boundary table from SPEC.md section 5.
	cases := []struct {
		hosts                    int
		size                     uint64
		prefix, usable, leftover int
	}{
		{1, 4, 30, 2, 1},
		{2, 4, 30, 2, 0},
		{3, 8, 29, 6, 3},
		{6, 8, 29, 6, 0},
		{7, 16, 28, 14, 7},
		{14, 16, 28, 14, 0},
		{30, 32, 27, 30, 0},
		{254, 256, 24, 254, 0},
		{255, 512, 23, 510, 255},
		{300, 512, 23, 510, 210},
		{510, 512, 23, 510, 0},
		{511, 1024, 22, 1022, 511},
	}
	for _, c := range cases {
		size := blockSize(c.hosts)
		if size != c.size {
			t.Errorf("blockSize(%d) = %d, want %d", c.hosts, size, c.size)
		}
		if prefix := addressBits - bits.Len64(size) + 1; prefix != c.prefix {
			t.Errorf("prefix for %d hosts = /%d, want /%d", c.hosts, prefix, c.prefix)
		}
		if usable := int(size) - unusablePerBlock; usable != c.usable {
			t.Errorf("usable for %d hosts = %d, want %d", c.hosts, usable, c.usable)
		}
		if leftover := int(size) - unusablePerBlock - c.hosts; leftover != c.leftover {
			t.Errorf("leftover for %d hosts = %d, want %d", c.hosts, leftover, c.leftover)
		}
	}
}

func TestAnExactFitDoesNotJumpToTheNextPrefix(t *testing.T) {
	if got := blockSize(254); got != 256 {
		t.Errorf("blockSize(254) = %d, want 256", got)
	}
	if got := blockSize(255); got != 512 {
		t.Errorf("blockSize(255) = %d, want 512", got)
	}
}

func TestBlockSizeInvariants(t *testing.T) {
	for _, hosts := range []int{1, 2, 3, 6, 7, 14, 30, 63, 64, 65, 253, 254, 255, 300, 1000} {
		size := blockSize(hosts)
		if size&(size-1) != 0 {
			t.Errorf("blockSize(%d) = %d is not a power of two", hosts, size)
		}
		if int(size)-unusablePerBlock < hosts {
			t.Errorf("blockSize(%d) = %d cannot hold %d hosts", hosts, size, hosts)
		}
		if int(size/2)-unusablePerBlock >= hosts {
			t.Errorf("blockSize(%d) = %d is not minimal", hosts, size)
		}
	}
}

func TestSmallestBlockIsASlash30(t *testing.T) {
	size := blockSize(1)
	if size != 4 {
		t.Fatalf("blockSize(1) = %d, want 4", size)
	}
	if prefix := addressBits - bits.Len64(size) + 1; prefix != 30 {
		t.Errorf("prefix = /%d, want /30", prefix)
	}
}

// --- allocation -----------------------------------------------------------

func TestReproducesTheSpecWorkedExample(t *testing.T) {
	want := []struct {
		name          string
		cidr          string
		usable, extra int
	}{
		{"Sales", "192.168.0.0/23", 510, 210},
		{"Engineering", "192.168.2.0/25", 126, 6},
		{"Warehouse", "192.168.2.128/26", 62, 2},
		{"Ops", "192.168.2.192/27", 30, 5},
		{"Guest", "192.168.2.224/28", 14, 4},
	}
	got := plan(t, exampleRows...)
	if len(got) != len(want) {
		t.Fatalf("got %d allocations, want %d", len(got), len(want))
	}
	for i, w := range want {
		a := got[i]
		if a.Subsystem.Name != w.name || a.CIDR() != w.cidr ||
			a.TotalUsable() != w.usable || a.Leftover() != w.extra {
			t.Errorf("row %d = %s %s %d %d, want %s %s %d %d", i,
				a.Subsystem.Name, a.CIDR(), a.TotalUsable(), a.Leftover(),
				w.name, w.cidr, w.usable, w.extra)
		}
	}
}

func TestNetworkAndBroadcastBracketTheUsableRange(t *testing.T) {
	for _, a := range plan(t, exampleRows...) {
		if value(a.FirstUsable()) != value(a.NetworkAddress())+1 {
			t.Errorf("%s: first usable is not one above the network address", a.Subsystem.Name)
		}
		if value(a.LastUsable()) != value(a.BroadcastAddress())-1 {
			t.Errorf("%s: last usable is not one below the broadcast address", a.Subsystem.Name)
		}
	}
}

func TestSalesSpansTwoThirdOctetValues(t *testing.T) {
	sales := plan(t, exampleRows...)[0]
	if got := sales.FirstUsable().String(); got != "192.168.0.1" {
		t.Errorf("first usable = %s, want 192.168.0.1", got)
	}
	if got := sales.LastUsable().String(); got != "192.168.1.254" {
		t.Errorf("last usable = %s, want 192.168.1.254", got)
	}
	if got := sales.BroadcastAddress().String(); got != "192.168.1.255" {
		t.Errorf("broadcast = %s, want 192.168.1.255", got)
	}
}

func TestSortsByBlockSizeNotByHostCount(t *testing.T) {
	// 100 and 120 hosts both need a /25, so they tie and must keep input order.
	// Sorting by host count would put 120 first.
	got := plan(t, [2]any{"Smaller", 100}, [2]any{"Larger", 120})
	if got[0].Subsystem.Name != "Smaller" || got[1].Subsystem.Name != "Larger" {
		t.Errorf("order = %s, %s; want Smaller, Larger",
			got[0].Subsystem.Name, got[1].Subsystem.Name)
	}
}

func TestLargerBlocksComeFirst(t *testing.T) {
	got := plan(t, [2]any{"Tiny", 5}, [2]any{"Huge", 900}, [2]any{"Middle", 100})
	want := []string{"Huge", "Middle", "Tiny"}
	for i, name := range want {
		if got[i].Subsystem.Name != name {
			t.Errorf("position %d = %s, want %s", i, got[i].Subsystem.Name, name)
		}
	}
}

func TestReversingTiedInputReversesOutput(t *testing.T) {
	forward := plan(t, [2]any{"Zulu", 25}, [2]any{"Alpha", 30}, [2]any{"Mike", 20})
	reverse := plan(t, [2]any{"Mike", 20}, [2]any{"Alpha", 30}, [2]any{"Zulu", 25})
	if forward[0].Subsystem.Name != "Zulu" || reverse[0].Subsystem.Name != "Mike" {
		t.Errorf("sort is not stable: forward starts %s, reverse starts %s",
			forward[0].Subsystem.Name, reverse[0].Subsystem.Name)
	}
}

func TestTieGroups(t *testing.T) {
	groups := tieGroups(plan(t, [2]any{"A", 25}, [2]any{"B", 30}, [2]any{"C", 100}, [2]any{"D", 120}))
	if len(groups) != 2 {
		t.Fatalf("got %d groups, want 2", len(groups))
	}
	if groups[0].Prefix != 25 || strings.Join(groups[0].Names, ",") != "C,D" {
		t.Errorf("first group = /%d %v, want /25 [C D]", groups[0].Prefix, groups[0].Names)
	}
	if groups[1].Prefix != 27 || strings.Join(groups[1].Names, ",") != "A,B" {
		t.Errorf("second group = /%d %v, want /27 [A B]", groups[1].Prefix, groups[1].Names)
	}
}

func TestNoTieGroupsWhenEveryBlockSizeDiffers(t *testing.T) {
	if groups := tieGroups(plan(t, exampleRows...)); len(groups) != 0 {
		t.Errorf("got %d groups, want 0", len(groups))
	}
}

func TestRejectsAMisalignedBase(t *testing.T) {
	_, err := allocate(subsystems(exampleRows...), netip.MustParseAddr("192.168.1.0"))
	var layoutErr *LayoutError
	if err == nil || !errors.As(err, &layoutErr) {
		t.Fatalf("got %v, want a LayoutError", err)
	}
	for _, want := range []string{"aligned", "192.168.0.0", "192.168.2.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %q", err, want)
		}
	}
}

func TestAcceptsAnAlignedBase(t *testing.T) {
	got := planAt(t, "192.168.2.0", exampleRows...)
	if got[0].CIDR() != "192.168.2.0/23" {
		t.Errorf("first block = %s, want 192.168.2.0/23", got[0].CIDR())
	}
}

func TestRejectsAPlanRunningPastTheEndOfIPv4(t *testing.T) {
	// The plan needs 752 addresses; the last /23-aligned base leaves 512.
	_, err := allocate(subsystems(exampleRows...), netip.MustParseAddr("255.255.254.0"))
	if err == nil || !strings.Contains(err.Error(), "255.255.255.255") {
		t.Fatalf("got %v, want an overflow LayoutError", err)
	}
}

func TestAPlanEndingExactlyAtTheLastAddressIsAllowed(t *testing.T) {
	got := planAt(t, "255.255.252.0", [2]any{"Big", 300}, [2]any{"Another", 300})
	if last := got[len(got)-1].BroadcastAddress().String(); last != "255.255.255.255" {
		t.Errorf("last broadcast = %s, want 255.255.255.255", last)
	}
}

func TestEmptyInputProducesNoAllocations(t *testing.T) {
	got, err := allocate(nil, defaultBaseAddr)
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v; want no allocations and no error", got, err)
	}
}

func TestAllocationInvariantsOverRandomPlans(t *testing.T) {
	for seed := range 60 {
		rng := rand.New(rand.NewSource(int64(seed)))
		rows := make([][2]any, 0, 12)
		for i := range rng.Intn(12) + 1 {
			rows = append(rows, [2]any{"sub" + strconv.Itoa(i), rng.Intn(4000) + 1})
		}
		got := planAt(t, "10.0.0.0", rows...)

		var consumed uint64
		for i, a := range got {
			if a.TotalUsable() < a.Subsystem.Hosts {
				t.Fatalf("seed %d: %s cannot hold its hosts", seed, a.Subsystem.Name)
			}
			if a.Leftover() < 0 {
				t.Fatalf("seed %d: %s has negative leftover", seed, a.Subsystem.Name)
			}
			if a.Network%a.Size != 0 {
				t.Fatalf("seed %d: %s is not aligned to its own size", seed, a.Subsystem.Name)
			}
			if i > 0 && value(got[i-1].BroadcastAddress())+1 != a.Network {
				t.Fatalf("seed %d: gap or overlap before %s", seed, a.Subsystem.Name)
			}
			consumed += a.Size
		}
		span := value(got[len(got)-1].BroadcastAddress()) - got[0].Network + 1
		if span != consumed {
			t.Fatalf("seed %d: span %d != consumed %d", seed, span, consumed)
		}
	}
}

// --- parsing --------------------------------------------------------------

func TestParsesTwoColumns(t *testing.T) {
	got, err := parseInput([]string{"Sales 300", "Engineering 120"})
	if err != nil {
		t.Fatalf("parseInput: %v", err)
	}
	want := []Subsystem{{"Sales", 300, 1}, {"Engineering", 120, 2}}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("row %d = %+v, want %+v", i, got[i], w)
		}
	}
}

func TestParsingAcceptsSeparatorsAndSpacedNames(t *testing.T) {
	cases := map[string]Subsystem{
		"Sales\t300":         {"Sales", 300, 1},
		"Sales,300":          {"Sales", 300, 1},
		"Sales      300":     {"Sales", 300, 1},
		"HR Department 45":   {"HR Department", 45, 1},
		"   Sales    300   ": {"Sales", 300, 1},
	}
	for input, want := range cases {
		got, err := parseInput([]string{input})
		if err != nil {
			t.Fatalf("parseInput(%q): %v", input, err)
		}
		if got[0] != want {
			t.Errorf("parseInput(%q) = %+v, want %+v", input, got[0], want)
		}
	}
}

func TestParsingSkipsCommentsAndBlanksAndRecordsLineNumbers(t *testing.T) {
	got, err := parseInput([]string{"# header", "", "Sales 300", "   ", "Ops 25"})
	if err != nil {
		t.Fatalf("parseInput: %v", err)
	}
	if len(got) != 2 || got[0].Line != 3 || got[1].Line != 5 {
		t.Errorf("got %+v, want Sales on line 3 and Ops on line 5", got)
	}
}

func TestParsingRejectsBadInput(t *testing.T) {
	cases := map[string]string{
		"Sales":     "line 1",
		"Sales 0":   "hosts",
		"Sales -5":  "hosts",
		"Sales abc": "hosts",
		"Sales 3.5": "hosts",
	}
	for input, want := range cases {
		_, err := parseInput([]string{input})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("parseInput(%q) = %v, want an error mentioning %q", input, err, want)
		}
	}
}

func TestParsingRejectsDuplicateNames(t *testing.T) {
	_, err := parseInput([]string{"Sales 300", "Ops 25", "Sales 120"})
	if err == nil {
		t.Fatal("want a duplicate-name error")
	}
	for _, want := range []string{"duplicate", "'Sales'", "line 3", "line 1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q does not mention %q", err, want)
		}
	}
}

func TestNamesDifferingOnlyInCaseAreNotDuplicates(t *testing.T) {
	got, err := parseInput([]string{"Sales 300", "sales 120"})
	if err != nil || len(got) != 2 {
		t.Errorf("got %v, %v; want two subsystems", got, err)
	}
}

func TestNamesDifferingOnlyInWhitespaceAreDuplicates(t *testing.T) {
	_, err := parseInput([]string{"Sales 300", "   Sales    120"})
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("got %v, want a duplicate-name error", err)
	}
}

// --- rendering ------------------------------------------------------------

func TestRenderTextReproducesTheGoldenTable(t *testing.T) {
	if got := renderText(plan(t, exampleRows...), defaultBaseAddr); got != goldenText {
		t.Errorf("render mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, goldenText)
	}
}

func TestRenderTextCarriesNoTrailingWhitespace(t *testing.T) {
	for _, line := range strings.Split(renderText(plan(t, exampleRows...), defaultBaseAddr), "\n") {
		if line != strings.TrimRight(line, " ") {
			t.Errorf("line has trailing whitespace: %q", line)
		}
	}
}

func TestRenderTextPlacesNetworkAndBroadcastEitherSideOfTheRange(t *testing.T) {
	header := strings.Split(renderText(plan(t, exampleRows...), defaultBaseAddr), "\n")[0]
	if !(strings.Index(header, "NETWORK") < strings.Index(header, "USABLE IP RANGE") &&
		strings.Index(header, "USABLE IP RANGE") < strings.Index(header, "BROADCAST")) {
		t.Errorf("column order is wrong: %q", header)
	}
}

func TestRenderCSV(t *testing.T) {
	rows := strings.Split(renderCSV(plan(t, exampleRows...)), "\n")
	wantHeader := "subsystem,hosts,network,usable_range,broadcast,cidr,total_hosts,leftover"
	if rows[0] != wantHeader {
		t.Errorf("header = %q, want %q", rows[0], wantHeader)
	}
	wantFirst := "Sales,300,192.168.0.0,192.168.0.1 - 192.168.1.254,192.168.1.255,192.168.0.0/23,510,210"
	if rows[1] != wantFirst {
		t.Errorf("first row = %q, want %q", rows[1], wantFirst)
	}
	if strings.Contains(renderCSV(plan(t, exampleRows...)), "Allocated") {
		t.Error("csv must not carry the summary line")
	}
}

func TestRenderCSVQuotesANameContainingAComma(t *testing.T) {
	if got := renderCSV(plan(t, [2]any{"Sales, EMEA", 10})); !strings.Contains(got, `"Sales, EMEA"`) {
		t.Errorf("got %q, want the name quoted", got)
	}
}

func TestRenderJSON(t *testing.T) {
	var payload struct {
		Base        string `json:"base"`
		Allocations []struct {
			Subsystem        string `json:"subsystem"`
			Hosts            int    `json:"hosts"`
			CIDR             string `json:"cidr"`
			NetworkAddress   string `json:"network_address"`
			BroadcastAddress string `json:"broadcast_address"`
			FirstUsable      string `json:"first_usable"`
			LastUsable       string `json:"last_usable"`
			TotalHosts       int    `json:"total_hosts"`
			Leftover         int    `json:"leftover"`
		} `json:"allocations"`
		Summary struct {
			TotalAddresses int `json:"total_addresses"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(renderJSON(plan(t, exampleRows...), defaultBaseAddr)), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Base != "192.168.0.0" || len(payload.Allocations) != 5 {
		t.Fatalf("got base %q with %d allocations", payload.Base, len(payload.Allocations))
	}
	if payload.Summary.TotalAddresses != 752 {
		t.Errorf("total addresses = %d, want 752", payload.Summary.TotalAddresses)
	}
	first := payload.Allocations[0]
	if first.NetworkAddress != "192.168.0.0" || first.BroadcastAddress != "192.168.1.255" {
		t.Errorf("first allocation = %+v", first)
	}
}

// --- CLI ------------------------------------------------------------------

func writeTemp(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}

func invoke(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, strings.NewReader(""))
	return code, stdout.String(), stderr.String()
}

func TestCLIPrintsTheGoldenTable(t *testing.T) {
	path := writeTemp(t, "subsystems.txt", exampleFile)
	code, stdout, stderr := invoke(t, path)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if strings.TrimRight(stdout, "\n") != goldenText {
		t.Errorf("stdout mismatch:\n--- got ---\n%s", stdout)
	}
}

func TestCLIReadsStdin(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"-"}, &stdout, &stderr, strings.NewReader(exampleFile))
	if code != 0 || !strings.Contains(stdout.String(), "192.168.0.0/23") {
		t.Errorf("exit = %d, stdout = %s, stderr = %s", code, stdout.String(), stderr.String())
	}
}

func TestCLIHonoursAnExplicitBase(t *testing.T) {
	path := writeTemp(t, "subsystems.txt", exampleFile)
	_, stdout, _ := invoke(t, "-base", "10.0.0.0", path)
	if !strings.Contains(stdout, "10.0.0.0/23") {
		t.Errorf("stdout = %s", stdout)
	}
}

func TestCLIFormats(t *testing.T) {
	path := writeTemp(t, "subsystems.txt", exampleFile)

	_, csvOut, _ := invoke(t, "-format", "csv", path)
	if !strings.HasPrefix(csvOut, "subsystem,hosts,network") {
		t.Errorf("csv stdout = %s", csvOut)
	}

	_, jsonOut, _ := invoke(t, "-format", "json", path)
	var payload map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &payload); err != nil {
		t.Errorf("json stdout is not valid JSON: %v", err)
	}
}

func TestCLIOrderInputRestoresFileOrder(t *testing.T) {
	path := writeTemp(t, "unsorted.txt", "Guest 10\nSales 300\nOps 25\n")
	_, stdout, _ := invoke(t, "-format", "csv", "-order", "input", path)

	names := []string{}
	for _, row := range strings.Split(strings.TrimRight(stdout, "\n"), "\n")[1:] {
		names = append(names, strings.Split(row, ",")[0])
	}
	if strings.Join(names, ",") != "Guest,Sales,Ops" {
		t.Errorf("order = %v, want Guest Sales Ops", names)
	}
	if !strings.Contains(stdout, "192.168.0.0/23") {
		t.Error("reordering must not change the blocks assigned")
	}
}

func TestCLIOutputFlagWritesAFile(t *testing.T) {
	path := writeTemp(t, "subsystems.txt", exampleFile)
	destination := filepath.Join(t.TempDir(), "plan.txt")

	code, stdout, _ := invoke(t, "-output", destination, path)
	if code != 0 || stdout != "" {
		t.Fatalf("exit = %d, stdout = %q", code, stdout)
	}
	written, err := os.ReadFile(destination)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.TrimRight(string(written), "\n") != goldenText {
		t.Errorf("written file mismatch:\n%s", written)
	}
}

func TestCLITieNotesGoToStderr(t *testing.T) {
	path := writeTemp(t, "ties.txt", "Ops 25\nLab 30\n")

	code, stdout, stderr := invoke(t, path)
	if code != 0 {
		t.Errorf("a tie must not change the exit code, got %d", code)
	}
	if !strings.Contains(stderr, "tie") || !strings.Contains(stderr, "Ops") ||
		!strings.Contains(stderr, "Lab") {
		t.Errorf("stderr = %q", stderr)
	}
	if strings.Contains(stdout, "tie") {
		t.Error("tie notes must not reach stdout")
	}

	_, _, quietErr := invoke(t, "-q", path)
	if quietErr != "" {
		t.Errorf("-q must suppress notes, got %q", quietErr)
	}
}

func TestCLINoNoteWhenNoBlockSizesTie(t *testing.T) {
	path := writeTemp(t, "subsystems.txt", exampleFile)
	if _, _, stderr := invoke(t, path); stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestCLIErrorExitCodes(t *testing.T) {
	example := writeTemp(t, "subsystems.txt", exampleFile)
	duplicates := writeTemp(t, "dupes.txt", "Sales 300\nOps 25\nSales 10\n")
	bad := writeTemp(t, "bad.txt", "Sales 300\nOps 0\n")

	cases := []struct {
		name string
		args []string
		code int
		want string
	}{
		{"duplicate name", []string{duplicates}, 2, "duplicate"},
		{"bad host count", []string{bad}, 2, "line 2"},
		{"base with prefix", []string{"-base", "192.168.0.0/24", example}, 2, "bare address"},
		{"malformed base", []string{"-base", "not-an-address", example}, 2, "--base"},
		{"missing file", []string{filepath.Join(t.TempDir(), "nope.txt")}, 2, "cannot read"},
		{"misaligned base", []string{"-base", "192.168.1.0", example}, 1, "aligned"},
		{"ipv4 overflow", []string{"-base", "255.255.254.0", example}, 1, "255.255.255.255"},
		{"bad format", []string{"-format", "yaml", example}, 2, "--format"},
		{"no input file", []string{}, 2, "exactly one input file"},
	}
	for _, c := range cases {
		code, stdout, stderr := invoke(t, c.args...)
		if code != c.code {
			t.Errorf("%s: exit = %d, want %d (stderr %q)", c.name, code, c.code, stderr)
		}
		if !strings.Contains(stderr, c.want) {
			t.Errorf("%s: stderr %q does not mention %q", c.name, stderr, c.want)
		}
		if stdout != "" {
			t.Errorf("%s: stdout must stay empty on error, got %q", c.name, stdout)
		}
	}
}

func TestCLIEmptyInputSucceeds(t *testing.T) {
	path := writeTemp(t, "empty.txt", "")
	code, stdout, _ := invoke(t, path)
	if code != 0 || !strings.Contains(stdout, "SUBSYSTEM") {
		t.Errorf("exit = %d, stdout = %q", code, stdout)
	}
}

func TestCLIUnwritableOutputPathExitsTwo(t *testing.T) {
	path := writeTemp(t, "subsystems.txt", exampleFile)
	unreachable := filepath.Join(t.TempDir(), "no-such-directory", "plan.txt")

	code, stdout, stderr := invoke(t, "-output", unreachable, path)
	if code != 2 || !strings.Contains(stderr, "cannot write") || stdout != "" {
		t.Errorf("exit = %d, stdout = %q, stderr = %q", code, stdout, stderr)
	}
}

func TestAFlagAfterTheFilenameIsReportedClearly(t *testing.T) {
	// Go's flag package stops at the first non-flag argument, so this is a
	// usage error rather than a working command. The message must say why.
	var stdout, stderr bytes.Buffer
	code := run([]string{"plan.txt", "-format", "csv"}, &stdout, &stderr, strings.NewReader(""))

	if code != 2 {
		t.Errorf("exit code = %d, want 2", code)
	}
	if !strings.Contains(stderr.String(), "flags must come before the input file") {
		t.Errorf("message does not explain flag ordering: %q", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout should be empty, got %q", stdout.String())
	}
}
