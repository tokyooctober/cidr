// Command cidrplan is a VLSM subnet allocator.
//
// It reads a two-column plan (subsystem, hosts) and assigns each subsystem a
// correctly-sized, non-overlapping CIDR block laid out contiguously from a base
// address. This is a port of the Python implementation at the repository root
// and produces identical output, except that it writes LF line endings where
// Python's print writes CRLF on Windows. See SPEC.md for the full specification.
package main

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/bits"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	addressBits = 32
	// Every block loses two addresses to the network and broadcast addresses,
	// so a block must be sized to hold the host count plus these two.
	unusablePerBlock = 2
	lastIPv4Address  = uint64(1)<<addressBits - 1
	defaultBase      = "192.168.0.0"
)

// ParseError means the input file could not be read as a valid plan.
type ParseError struct{ msg string }

func (e *ParseError) Error() string { return e.msg }

// LayoutError means a valid plan could not be laid out at the requested base.
type LayoutError struct{ msg string }

func (e *LayoutError) Error() string { return e.msg }

// Subsystem is one row of the input file.
type Subsystem struct {
	Name  string
	Hosts int
	Line  int
}

// Allocation is a subsystem and the block assigned to it.
type Allocation struct {
	Subsystem Subsystem
	Network   uint64
	Size      uint64
}

// PrefixLen is the CIDR prefix length of the assigned block.
func (a Allocation) PrefixLen() int { return addressBits - bits.Len64(a.Size) + 1 }

// NetworkAddress is the first address of the block. Not assignable to a host.
func (a Allocation) NetworkAddress() netip.Addr { return addr(a.Network) }

// BroadcastAddress is the last address of the block. Not assignable to a host.
func (a Allocation) BroadcastAddress() netip.Addr { return addr(a.Network + a.Size - 1) }

// FirstUsable is the lowest address assignable to a host.
func (a Allocation) FirstUsable() netip.Addr { return addr(a.Network + 1) }

// LastUsable is the highest address assignable to a host.
func (a Allocation) LastUsable() netip.Addr { return addr(a.Network + a.Size - 2) }

// TotalUsable is the addresses available to hosts, excluding network and broadcast.
func (a Allocation) TotalUsable() int { return int(a.Size) - unusablePerBlock }

// Leftover is the capacity beyond the requested host count.
func (a Allocation) Leftover() int { return a.TotalUsable() - a.Subsystem.Hosts }

// CIDR renders the assigned block in prefix notation.
func (a Allocation) CIDR() string {
	return fmt.Sprintf("%s/%d", a.NetworkAddress(), a.PrefixLen())
}

// addr converts a 32-bit address value to an IPv4 address.
func addr(v uint64) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// blockSize returns the smallest power-of-two block holding hosts usable
// addresses. It guarantees usable capacity >= hosts, so leftover is never
// negative.
func blockSize(hosts int) uint64 {
	totalNeeded := uint64(hosts) + unusablePerBlock
	size := uint64(1)
	for size < totalNeeded {
		size *= 2
	}
	return size
}

// allocate assigns each subsystem a block, laid out contiguously from base.
//
// Largest block first: this is what keeps every block on its own power-of-two
// boundary, so the plan has no alignment gaps. Ties keep input order via a
// stable sort — different host counts routinely share a block size (100 and 120
// both need a /25), so a secondary key would silently reorder them.
func allocate(subsystems []Subsystem, base netip.Addr) ([]Allocation, error) {
	if len(subsystems) == 0 {
		return nil, nil
	}

	ordered := make([]Subsystem, len(subsystems))
	copy(ordered, subsystems)
	sort.SliceStable(ordered, func(i, j int) bool {
		return blockSize(ordered[i].Hosts) > blockSize(ordered[j].Hosts)
	})

	largest := blockSize(ordered[0].Hosts)
	cursor := value(base)
	if largest > lastIPv4Address+1 || cursor%largest != 0 {
		below := (cursor / largest) * largest
		above := below + largest
		return nil, &LayoutError{fmt.Sprintf(
			"base %s is not aligned to the largest block (/%d, %d addresses). "+
				"Nearest valid starting addresses: %s or %s.",
			base, addressBits-bits.Len64(largest)+1, largest, addr(below), addr(above),
		)}
	}

	var total uint64
	for _, s := range ordered {
		total += blockSize(s.Hosts)
	}
	if cursor+total-1 > lastIPv4Address {
		return nil, &LayoutError{fmt.Sprintf(
			"plan needs %d addresses starting at %s, which runs past "+
				"255.255.255.255. Choose a lower base address.", total, base,
		)}
	}

	allocations := make([]Allocation, 0, len(ordered))
	for _, subsystem := range ordered {
		size := blockSize(subsystem.Hosts)
		a := Allocation{Subsystem: subsystem, Network: cursor, Size: size}
		// A negative leftover would mean the block is smaller than the host
		// count it was sized for, which is a bug rather than a user error.
		if a.Leftover() < 0 {
			panic("negative leftover for " + subsystem.Name)
		}
		allocations = append(allocations, a)
		cursor += size
	}
	return allocations, nil
}

// value converts an IPv4 address to its 32-bit numeric form.
func value(a netip.Addr) uint64 {
	b := a.As4()
	return uint64(b[0])<<24 | uint64(b[1])<<16 | uint64(b[2])<<8 | uint64(b[3])
}

// TieGroup is a prefix length shared by more than one subsystem.
type TieGroup struct {
	Prefix int
	Names  []string
}

// tieGroups reports prefix lengths shared by more than one subsystem, longest
// block first.
func tieGroups(allocations []Allocation) []TieGroup {
	byPrefix := map[int][]string{}
	for _, a := range allocations {
		byPrefix[a.PrefixLen()] = append(byPrefix[a.PrefixLen()], a.Subsystem.Name)
	}
	prefixes := make([]int, 0, len(byPrefix))
	for p := range byPrefix {
		prefixes = append(prefixes, p)
	}
	sort.Ints(prefixes)

	groups := []TieGroup{}
	for _, p := range prefixes {
		if len(byPrefix[p]) > 1 {
			groups = append(groups, TieGroup{Prefix: p, Names: byPrefix[p]})
		}
	}
	return groups
}

var textHeaders = []string{
	"SUBSYSTEM",
	"HOSTS",
	"NETWORK",
	"USABLE IP RANGE",
	"BROADCAST",
	"CIDR (TOTAL HOSTS)",
	"LEFTOVER CAPACITY",
}

var rightAlignedColumns = map[int]bool{1: true, 6: true}

// textRow renders one allocation as table cells.
//
// Network and broadcast bracket the usable range, so the block reads left to
// right: what it starts on, what is assignable, what it ends on.
func textRow(a Allocation) []string {
	return []string{
		a.Subsystem.Name,
		strconv.Itoa(a.Subsystem.Hosts),
		a.NetworkAddress().String(),
		fmt.Sprintf("%s - %s", a.FirstUsable(), a.LastUsable()),
		a.BroadcastAddress().String(),
		fmt.Sprintf("%s (%d hosts)", a.CIDR(), a.TotalUsable()),
		strconv.Itoa(a.Leftover()),
	}
}

// pad widens s to width, counting runes rather than bytes so that the column
// arithmetic matches the Python implementation.
func pad(s string, width int, right bool) string {
	gap := width - utf8.RuneCountInString(s)
	if gap < 0 {
		gap = 0
	}
	if right {
		return strings.Repeat(" ", gap) + s
	}
	return s + strings.Repeat(" ", gap)
}

func summaryLines(allocations []Allocation, base netip.Addr) []string {
	var consumed uint64
	for _, a := range allocations {
		consumed += a.Size
	}
	end := value(base) + consumed
	first := fmt.Sprintf("Allocated 0 addresses from %s.", base)
	if consumed > 0 {
		first = fmt.Sprintf("Allocated %d addresses from %s through %s.",
			consumed, base, addr(end-1))
	}
	return []string{first, fmt.Sprintf("Next free address: %s", addr(end))}
}

// renderText renders the aligned report table plus its summary.
func renderText(allocations []Allocation, base netip.Addr) string {
	rows := make([][]string, 0, len(allocations))
	for _, a := range allocations {
		rows = append(rows, textRow(a))
	}

	widths := make([]int, len(textHeaders))
	for i, header := range textHeaders {
		widths[i] = utf8.RuneCountInString(header)
		for _, row := range rows {
			if n := utf8.RuneCountInString(row[i]); n > widths[i] {
				widths[i] = n
			}
		}
	}

	line := func(cells []string) string {
		padded := make([]string, len(cells))
		for i, cell := range cells {
			padded[i] = pad(cell, widths[i], rightAlignedColumns[i])
		}
		return strings.TrimRight(strings.Join(padded, "  "), " ")
	}

	body := []string{line(textHeaders)}
	for _, row := range rows {
		body = append(body, line(row))
	}
	body = append(body, "")
	body = append(body, summaryLines(allocations, base)...)
	return strings.Join(body, "\n")
}

// renderCSV renders the report as CSV, with the CIDR and its capacity split apart.
func renderCSV(allocations []Allocation) string {
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	_ = writer.Write([]string{
		"subsystem", "hosts", "network", "usable_range",
		"broadcast", "cidr", "total_hosts", "leftover",
	})
	for _, a := range allocations {
		_ = writer.Write([]string{
			a.Subsystem.Name,
			strconv.Itoa(a.Subsystem.Hosts),
			a.NetworkAddress().String(),
			fmt.Sprintf("%s - %s", a.FirstUsable(), a.LastUsable()),
			a.BroadcastAddress().String(),
			a.CIDR(),
			strconv.Itoa(a.TotalUsable()),
			strconv.Itoa(a.Leftover()),
		})
	}
	writer.Flush()
	return strings.TrimRight(buffer.String(), "\n")
}

// jsonAllocation mirrors the Python payload; field order is the emitted order.
type jsonAllocation struct {
	Subsystem        string `json:"subsystem"`
	Hosts            int    `json:"hosts"`
	CIDR             string `json:"cidr"`
	NetworkAddress   string `json:"network_address"`
	BroadcastAddress string `json:"broadcast_address"`
	FirstUsable      string `json:"first_usable"`
	LastUsable       string `json:"last_usable"`
	TotalHosts       int    `json:"total_hosts"`
	Leftover         int    `json:"leftover"`
}

type jsonSummary struct {
	TotalAddresses uint64 `json:"total_addresses"`
	NextFree       string `json:"next_free"`
}

type jsonPayload struct {
	Base        string           `json:"base"`
	Allocations []jsonAllocation `json:"allocations"`
	Summary     jsonSummary      `json:"summary"`
}

// renderJSON renders the report as JSON.
func renderJSON(allocations []Allocation, base netip.Addr) string {
	var consumed uint64
	items := []jsonAllocation{}
	for _, a := range allocations {
		consumed += a.Size
		items = append(items, jsonAllocation{
			Subsystem:        a.Subsystem.Name,
			Hosts:            a.Subsystem.Hosts,
			CIDR:             a.CIDR(),
			NetworkAddress:   a.NetworkAddress().String(),
			BroadcastAddress: a.BroadcastAddress().String(),
			FirstUsable:      a.FirstUsable().String(),
			LastUsable:       a.LastUsable().String(),
			TotalHosts:       a.TotalUsable(),
			Leftover:         a.Leftover(),
		})
	}
	payload := jsonPayload{
		Base:        base.String(),
		Allocations: items,
		Summary: jsonSummary{
			TotalAddresses: consumed,
			NextFree:       addr(value(base) + consumed).String(),
		},
	}

	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	// Match Python's json.dumps, which neither escapes HTML nor appends a newline.
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	_ = encoder.Encode(payload)
	return strings.TrimRight(buffer.String(), "\n")
}

// parseCount parses a field that must be a whole number.
func parseCount(raw, field string, lineNumber int) (int, error) {
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, &ParseError{fmt.Sprintf(
			"line %d: %s must be an integer, got '%s'", lineNumber, field, raw)}
	}
	return n, nil
}

// parseInput parses two-column plan text into subsystems.
//
// The last field of a line is the host count; everything before it is the name,
// so names may contain spaces without needing quoting.
func parseInput(lines []string) ([]Subsystem, error) {
	subsystems := []Subsystem{}
	firstSeen := map[string]int{}

	for i, rawLine := range lines {
		lineNumber := i + 1
		stripped := strings.TrimSpace(rawLine)
		if stripped == "" || strings.HasPrefix(stripped, "#") {
			continue
		}

		fields := strings.Fields(strings.ReplaceAll(stripped, ",", " "))
		if len(fields) < 2 {
			return nil, &ParseError{fmt.Sprintf(
				"line %d: expected 2 fields (name, hosts), got %d: '%s'",
				lineNumber, len(fields), stripped)}
		}

		name := strings.Join(fields[:len(fields)-1], " ")
		hosts, err := parseCount(fields[len(fields)-1], "hosts", lineNumber)
		if err != nil {
			return nil, err
		}
		if hosts < 1 {
			return nil, &ParseError{fmt.Sprintf(
				"line %d: hosts must be a positive integer, got %d", lineNumber, hosts)}
		}

		// Checked across the whole file before any allocation happens, so a
		// duplicate on the last line fails before a partial plan is printed.
		if seen, ok := firstSeen[name]; ok {
			return nil, &ParseError{fmt.Sprintf(
				"duplicate subsystem name '%s' on line %d (first seen on line %d)",
				name, lineNumber, seen)}
		}
		firstSeen[name] = lineNumber

		subsystems = append(subsystems, Subsystem{Name: name, Hosts: hosts, Line: lineNumber})
	}
	return subsystems, nil
}

// parseBase parses the -base argument, rejecting anything carrying a prefix.
func parseBase(raw string) (netip.Addr, error) {
	if strings.Contains(raw, "/") {
		return netip.Addr{}, &ParseError{fmt.Sprintf(
			"--base takes a bare address, not a network. Prefix lengths are "+
				"computed from the host counts. Use --base %s.",
			strings.SplitN(raw, "/", 2)[0])}
	}
	parsed, err := netip.ParseAddr(raw)
	if err != nil || !parsed.Is4() {
		return netip.Addr{}, &ParseError{fmt.Sprintf(
			"--base is not a valid IPv4 address: '%s'", raw)}
	}
	return parsed, nil
}

func readLines(source string, stdin io.Reader) ([]string, error) {
	var reader io.Reader = stdin
	if source != "-" {
		file, err := os.Open(source)
		if err != nil {
			return nil, &ParseError{fmt.Sprintf("cannot read %s: %s", source, reason(err))}
		}
		defer file.Close()
		reader = file
	}

	lines := []string{}
	scanner := bufio.NewScanner(reader)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, &ParseError{fmt.Sprintf("cannot read %s: %s", source, reason(err))}
	}
	return lines, nil
}

// reason strips the operation and path prefix that os errors carry, leaving the
// underlying cause so messages read like the Python version's strerror.
func reason(err error) string {
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return pathErr.Err.Error()
	}
	return err.Error()
}

// run is the entry point body. It returns the process exit code.
func run(args []string, stdout, stderr io.Writer, stdin io.Reader) int {
	flags := flag.NewFlagSet("cidrplan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	base := flags.String("base", defaultBase,
		"starting address, bare with no prefix")
	format := flags.String("format", "text", "output format: text, csv or json")
	order := flags.String("order", "size", "report order: size or input")
	output := flags.String("output", "", "write the report here instead of stdout")
	var quiet bool
	flags.BoolVar(&quiet, "q", false, "suppress tie notes on stderr")
	flags.BoolVar(&quiet, "quiet", false, "suppress tie notes on stderr")

	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		// Go's flag package stops at the first non-flag argument, so a flag
		// written after the filename is silently swallowed as a positional.
		// Say so rather than reporting a confusing argument count.
		for i, arg := range flags.Args() {
			if i == 0 {
				continue
			}
			if strings.HasPrefix(arg, "-") && arg != "-" {
				fmt.Fprintf(stderr,
					"error: flags must come before the input file, so %s was not read as a flag.\n"+
						"       Usage: cidrplan [flags] <input-file>\n", arg)
				return 2
			}
		}
		fmt.Fprintln(stderr, "error: expected exactly one input file, or - for stdin")
		return 2
	}
	if *format != "text" && *format != "csv" && *format != "json" {
		fmt.Fprintf(stderr, "error: --format must be text, csv or json, got %q\n", *format)
		return 2
	}
	if *order != "size" && *order != "input" {
		fmt.Fprintf(stderr, "error: --order must be size or input, got %q\n", *order)
		return 2
	}

	baseAddr, err := parseBase(*base)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 2
	}

	lines, err := readLines(flags.Arg(0), stdin)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 2
	}

	subsystems, err := parseInput(lines)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 2
	}

	allocations, err := allocate(subsystems, baseAddr)
	if err != nil {
		fmt.Fprintf(stderr, "error: %s\n", err)
		return 1
	}

	if !quiet {
		for _, group := range tieGroups(allocations) {
			fmt.Fprintf(stderr, "note: %d subsystems tie at /%d (%s); allocated in input file order.\n",
				len(group.Names), group.Prefix, strings.Join(group.Names, ", "))
		}
	}

	if *order == "input" {
		sort.SliceStable(allocations, func(i, j int) bool {
			return allocations[i].Subsystem.Line < allocations[j].Subsystem.Line
		})
	}

	var report string
	switch *format {
	case "csv":
		report = renderCSV(allocations)
	case "json":
		report = renderJSON(allocations, baseAddr)
	default:
		report = renderText(allocations, baseAddr)
	}

	if *output != "" {
		if err := os.WriteFile(*output, []byte(report+"\n"), 0o644); err != nil {
			fmt.Fprintf(stderr, "error: cannot write %s: %s\n", *output, reason(err))
			return 2
		}
		return 0
	}

	fmt.Fprintln(stdout, report)
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, os.Stdin))
}
