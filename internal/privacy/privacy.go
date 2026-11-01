// Package privacy turns a snapshot into the exact bytes a user reviews and may publish.
//
// Order: drop identifying families by name, redact patterns in what remains,
// then enforce a size budget by omitting whole families. The output is always valid JSON.
package privacy

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/projectbluefin/bluefin-report/internal/snapshot"
)

// MaxBytes is the default attachment budget: 1 MiB minus room for the ```json fence
// publish wraps it in, so the GitHub artifact body stays under 1 MiB.
const MaxBytes = 1<<20 - 4<<10

// Options are the user's choices in Review.
type Options struct {
	Username    string            // login name to scrub wherever it appears
	IncludeUser bool              // attach the user-scope report
	KeepFiles   []string          // io.systemd.Files.* families the user explicitly opted in
	MaxBytes    int               // 0 means MaxBytes
	Extras      map[string]string // opt-in sections (extras.Sections keys); redacted like metrics
}

// Payload is what Review shows and Publish sends, byte for byte.
type Payload struct {
	JSON    []byte
	Removed map[string]int    // rule → number of values removed or rewritten
	Omitted []string          // families dropped to fit the budget
	Extras  map[string]string // Options.Extras after redaction, for the issue body
	// Summary is the snapshot summary with its unit names and warnings redacted, for the
	// issue body. Unit names carry identities too (var-home-<user>-….mount, sshd@…-<ip>:<port>.service).
	Summary snapshot.Summary
}

type document struct {
	MediaType string `json:"mediaType"`
	// Extras comes first so the size-limited Review preview always shows the opt-in diagnostics.
	Extras          map[string]string `json:"extras,omitempty"`
	System          snapshot.Report   `json:"system"`
	User            *snapshot.Report  `json:"user,omitempty"`
	OmittedFamilies []string          `json:"omittedFamilies,omitempty"`
}

// dropped families identify the machine and have no diagnostic value for a public report.
var droppedExact = []string{
	"io.systemd.Basic.Hostname",
	"io.systemd.Basic.MachineID",
	"io.systemd.Basic.BootID",
	// SMBIOS values unique to one physical machine; the make/model/firmware fields stay.
	"io.systemd.Basic.SMBIOS.SystemSerialNumber",
	"io.systemd.Basic.SMBIOS.SystemUUID",
	"io.systemd.Basic.SMBIOS.BaseBoardSerial",
	"io.systemd.Basic.SMBIOS.ChassisSerialNumber",
	"io.systemd.Basic.SMBIOS.BaseBoardAssetTag",
	"io.systemd.Basic.SMBIOS.ChassisAssetTagNumber",
}

var droppedPrefix = []string{
	"io.systemd.Basic.MachineInfo.",
	"io.systemd.Files.",  // privileged reads of arbitrary files; opt-in per family
	"io.systemd.Report.", // report signers publish per-machine keys, which link every report to one machine
}

var (
	reMAC   = regexp.MustCompile(`(?i)\b[0-9a-f]{2}(?::[0-9a-f]{2}){5}\b`)
	reIPish = regexp.MustCompile(`[0-9A-Fa-f.:]*[.:][0-9A-Fa-f.:]*`)
	reEmail = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	reHome  = regexp.MustCompile(`(?:/var)?/home/[^/\s"]+`)
	reUnit  = regexp.MustCompile(`\.(service|socket|target|mount|automount|swap|timer|path|slice|scope|device)$`)
)

var hostname = os.Hostname

// Build produces the reviewed payload from a snapshot. It refuses to return a
// payload unless a hostname identity can be resolved locally or from a report.
func Build(s snapshot.Snapshot, opt Options) (Payload, error) {
	if opt.MaxBytes == 0 {
		opt.MaxBytes = MaxBytes
	}
	r := redactor{removed: map[string]int{}}
	// The system report may be missing or rejected; os.Hostname still catches the
	// hostname in user metrics and extras.
	hosts := []string{}
	if h, err := hostname(); err == nil {
		hosts = append(hosts, h)
	}
	for _, report := range []snapshot.Report{s.System, s.User} {
		for _, m := range report.Metrics {
			if m.Name == "io.systemd.Basic.Hostname" {
				var reported string
				if json.Unmarshal(m.Value, &reported) == nil {
					hosts = append(hosts, reported)
				}
			}
		}
	}
	// Longest first, so "orca" never leaves the "-desk" of "orca-desk" behind.
	slices.SortFunc(hosts, func(a, b string) int { return cmp.Or(len(b)-len(a), strings.Compare(a, b)) })
	for _, h := range slices.Compact(hosts) {
		r.literal("hostname", h)
	}
	if len(r.literals) == 0 {
		return Payload{}, fmt.Errorf("cannot prepare a private report: hostname identity unavailable")
	}
	// Dropping the MachineID and BootID families by name is not enough: their values
	// recur inside other metrics, such as the journal entry carried by
	// io.systemd.Journal.HighPriorityMessage (_MACHINE_ID, _BOOT_ID, __CURSOR).
	for _, id := range []struct{ name, rule string }{
		{"io.systemd.Basic.MachineID", "machine ID"},
		{"io.systemd.Basic.BootID", "boot ID"},
	} {
		seen := []string{}
		for _, report := range []snapshot.Report{s.System, s.User} {
			for _, m := range report.Metrics {
				var reported string
				if m.Name == id.name && json.Unmarshal(m.Value, &reported) == nil && !slices.Contains(seen, reported) {
					seen = append(seen, reported)
					r.literal(id.rule, reported)
				}
			}
		}
	}
	r.literal("username", opt.Username)

	doc := document{MediaType: s.System.MediaType, System: r.report(s.System, opt.KeepFiles)}
	if opt.IncludeUser {
		u := r.report(s.User, opt.KeepFiles)
		doc.User = &u
	}
	if len(opt.Extras) > 0 {
		doc.Extras = map[string]string{}
		for k, v := range opt.Extras {
			doc.Extras[k] = r.text(v)
		}
	}
	// The issue body lists failed units from both scopes whether or not the user report is attached.
	scrubbed := s
	scrubbed.Warnings = r.texts(s.Warnings)
	sum := scrubbed.Summary() // derives Unreachable from the scrubbed warnings
	sum.FailedUnits = r.texts(sum.FailedUnits)

	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return Payload{}, err
	}
	for len(out) > opt.MaxBytes {
		fam := largestFamily(doc)
		if fam == "" {
			return Payload{}, fmt.Errorf("report is %d bytes with no metrics left to omit", len(out))
		}
		doc.System.Metrics = without(doc.System.Metrics, fam)
		if doc.User != nil {
			doc.User.Metrics = without(doc.User.Metrics, fam)
		}
		doc.OmittedFamilies = append(doc.OmittedFamilies, fam)
		if out, err = json.MarshalIndent(doc, "", "  "); err != nil {
			return Payload{}, err
		}
	}
	return Payload{JSON: out, Removed: r.removed, Omitted: doc.OmittedFamilies, Extras: doc.Extras, Summary: sum}, nil
}

type redactor struct {
	literals []*regexp.Regexp
	names    []string
	removed  map[string]int
}

// literal scrubs an exact word (hostname, username), including the form systemd uses
// in unit names, where "-" is escaped as `\x2d`. Word boundaries keep short known
// identities from replacing fragments of diagnostic words.
func (r *redactor) literal(rule, v string) {
	if strings.TrimSpace(v) == "" {
		return
	}
	for _, form := range slices.Compact([]string{v, strings.ReplaceAll(v, "-", `\x2d`)}) {
		r.literals = append(r.literals, regexp.MustCompile(`\b`+regexp.QuoteMeta(form)+`\b`))
		r.names = append(r.names, rule)
	}
}

func (r *redactor) report(in snapshot.Report, keep []string) snapshot.Report {
	out := snapshot.Report{MediaType: in.MediaType, Timestamp: in.Timestamp}
	for _, m := range in.Metrics {
		if dropped(m.Name) && !slices.Contains(keep, m.Name) {
			r.removed["identifying metric"]++
			continue
		}
		net := strings.HasPrefix(m.Name, "io.systemd.Network.")
		m.Object = r.text(r.virtualIf(net, m.Object))
		if fields, changed := r.value(m.Fields, net); changed {
			m.Fields = fields.(map[string]any)
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(m.Value))
		decoder.UseNumber()
		if json.Valid(m.Value) && decoder.Decode(&value) == nil {
			if scrubbed, changed := r.value(value, false); changed {
				m.Value, _ = json.Marshal(scrubbed)
			}
		}
		out.Metrics = append(out.Metrics, m)
	}
	return out
}

// value copies only changed containers, leaving the snapshot and untouched JSON
// values intact. Numbers decoded from metric values retain their original tokens.
func (r *redactor) value(v any, network bool) (any, bool) {
	switch v := v.(type) {
	case string:
		out := r.text(r.virtualIf(network, v))
		return out, out != v
	case map[string]any:
		out := v
		changed := false
		for key, value := range v {
			if scrubbed, modified := r.value(value, network); modified {
				if !changed {
					out = maps.Clone(v)
					changed = true
				}
				out[key] = scrubbed
			}
		}
		return out, changed
	case []any:
		out := v
		changed := false
		for i, value := range v {
			if scrubbed, modified := r.value(value, network); modified {
				if !changed {
					out = slices.Clone(v)
					changed = true
				}
				out[i] = scrubbed
			}
		}
		return out, changed
	default:
		return v, false
	}
}

// reVirtualIf matches VPN/container/bridge interface names, which reveal network setup.
var reVirtualIf = regexp.MustCompile(`^(lo|tun[0-9]*|tap[0-9]*|utun[0-9]*|wg[0-9]*|tailscale[0-9]*|proton[0-9]*|nordlynx|ipsec[0-9]*|zt[a-z0-9]*|ppp[0-9]*|docker[0-9]*|veth.*|virbr[0-9]*|vnet[0-9]*|br-.*|cni[0-9]*|flannel.*|kube.*)$`)

func (r *redactor) virtualIf(network bool, s string) string {
	if !network || !reVirtualIf.MatchString(s) {
		return s
	}
	r.removed["virtual interface"]++
	return "[virtual-if]"
}

func dropped(name string) bool {
	if slices.Contains(droppedExact, name) {
		return true
	}
	for _, p := range droppedPrefix {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (r *redactor) text(s string) string {
	if s == "" {
		return s
	}
	s = reEmail.ReplaceAllStringFunc(s, func(tok string) string {
		if reUnit.MatchString(tok) { // getty@tty1.service is a template unit, not an address
			return tok
		}
		r.removed["email"]++
		return "[email]"
	})
	s = r.replace(reHome, s, "home path", "~")
	s = r.replace(reMAC, s, "MAC address", "[mac]") // before IPv6: a MAC also parses as hex groups
	s = r.addresses(s)
	for i, re := range r.literals {
		s = r.replace(re, s, r.names[i], "["+r.names[i]+"]")
	}
	return s
}

// texts scrubs a copy of each string, leaving the input slice untouched.
func (r *redactor) texts(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, len(in))
	for i, v := range in {
		out[i] = r.text(v)
	}
	return out
}

func (r *redactor) addresses(s string) string {
	var out strings.Builder
	written := 0
	for offset := 0; offset < len(s); {
		match := reIPish.FindStringIndex(s[offset:])
		if match == nil {
			break
		}
		start, end := offset+match[0], offset+match[1]
		offset = end
		tok := s[start:end]
		candidate := tok
		ip := net.ParseIP(candidate)
		if ip == nil {
			// Keep meaningful IPv6 compression before trying colon punctuation.
			candidate = strings.Trim(tok, ".")
			ip = net.ParseIP(candidate)
		}
		if ip == nil {
			candidate = strings.Trim(tok, ".:")
			ip = net.ParseIP(candidate)
		}
		if ip == nil {
			// Bare IPv4 endpoints need their port split off, not trimmed away.
			// Unbracketed IPv6 is parsed above as a complete address.
			if host, _, err := net.SplitHostPort(candidate); err == nil {
				ip = net.ParseIP(host)
				if ip != nil && ip.To4() != nil {
					candidate = host
				} else {
					ip = nil
				}
			}
		}
		if ip == nil {
			continue // versions, times, unit names
		}
		replacement, rule := "[ipv6]", "IPv6 address"
		if ip.To4() != nil {
			replacement, rule = "[ipv4]", "IPv4 address"
		}
		at := start + strings.Index(tok, candidate)
		// A hex-looking fragment inside a word is diagnostic text, not an IP:
		// std::string, core::panicking, and Unicode identifiers must survive.
		// Check around the parsed address so separators like "ip:" still count.
		before, _ := utf8.DecodeLastRuneInString(s[:at])
		after, _ := utf8.DecodeRuneInString(s[at+len(candidate):])
		if ipWord(before) || ipWord(after) {
			continue
		}
		r.removed[rule]++
		out.WriteString(s[written:at])
		out.WriteString(replacement)
		written = at + len(candidate)
	}
	if written == 0 {
		return s
	}
	out.WriteString(s[written:])
	return out.String()
}

func ipWord(r rune) bool {
	return unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.Pc) || r == '\u200c' || r == '\u200d'
}

func (r *redactor) replace(re *regexp.Regexp, s, rule, with string) string {
	return re.ReplaceAllStringFunc(s, func(string) string {
		r.removed[rule]++
		return with
	})
}

// largestFamily returns the metric family contributing the most bytes, ties by name.
func largestFamily(d document) string {
	size := map[string]int{}
	add := func(ms []snapshot.Metric) {
		for _, m := range ms {
			b, _ := json.Marshal(m)
			size[m.Name] += len(b)
		}
	}
	add(d.System.Metrics)
	if d.User != nil {
		add(d.User.Metrics)
	}
	names := make([]string, 0, len(size))
	for n := range size {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if size[names[i]] != size[names[j]] {
			return size[names[i]] > size[names[j]]
		}
		return names[i] < names[j]
	})
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

func without(ms []snapshot.Metric, fam string) []snapshot.Metric {
	return slices.DeleteFunc(slices.Clone(ms), func(m snapshot.Metric) bool { return m.Name == fam })
}
