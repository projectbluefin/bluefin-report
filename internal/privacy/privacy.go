// Package privacy turns a snapshot into the exact bytes a user reviews and may publish.
//
// Order: drop identifying families by name, redact patterns in what remains,
// then enforce a size budget by omitting whole families. The output is always valid JSON.
package privacy

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/projectbluefin/bluefin-report/internal/snapshot"
)

// MaxBytes is the default attachment budget.
const MaxBytes = 1 << 20

// Options are the user's choices in Review.
type Options struct {
	Username    string   // login name to scrub wherever it appears
	IncludeUser bool     // attach the user-scope report
	KeepFiles   []string // io.systemd.Files.* families the user explicitly opted in
	MaxBytes    int      // 0 means MaxBytes
}

// Payload is what Review shows and Publish sends, byte for byte.
type Payload struct {
	JSON    []byte
	Removed map[string]int // rule → number of values removed or rewritten
	Omitted []string       // families dropped to fit the budget
}

type document struct {
	MediaType       string           `json:"mediaType"`
	System          snapshot.Report  `json:"system"`
	User            *snapshot.Report `json:"user,omitempty"`
	OmittedFamilies []string         `json:"omittedFamilies,omitempty"`
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

// Build produces the reviewed payload from a snapshot.
func Build(s snapshot.Snapshot, opt Options) (Payload, error) {
	if opt.MaxBytes == 0 {
		opt.MaxBytes = MaxBytes
	}
	r := redactor{removed: map[string]int{}}
	for _, m := range s.System.Metrics {
		if m.Name == "io.systemd.Basic.Hostname" {
			r.literal("hostname", m.String())
		}
	}
	r.literal("username", opt.Username)

	doc := document{MediaType: s.System.MediaType, System: r.report(s.System, opt.KeepFiles)}
	if opt.IncludeUser {
		u := r.report(s.User, opt.KeepFiles)
		doc.User = &u
	}

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
	return Payload{JSON: out, Removed: r.removed, Omitted: doc.OmittedFamilies}, nil
}

type redactor struct {
	literals []*regexp.Regexp
	names    []string
	removed  map[string]int
}

// literal scrubs an exact word (hostname, username), including the form systemd uses
// in unit names, where "-" is escaped as `\x2d`. Very short values are skipped:
// replacing "a" everywhere would destroy the report.
func (r *redactor) literal(rule, v string) {
	if len(v) < 3 {
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
		m.Object = r.text(m.Object)
		if len(m.Fields) > 0 {
			f := make(map[string]any, len(m.Fields))
			for k, v := range m.Fields {
				if s, ok := v.(string); ok {
					v = r.text(s)
				}
				f[k] = v
			}
			m.Fields = f
		}
		var s string
		if json.Unmarshal(m.Value, &s) == nil {
			m.Value, _ = json.Marshal(r.text(s))
		}
		out.Metrics = append(out.Metrics, m)
	}
	return out
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
	s = reIPish.ReplaceAllStringFunc(s, func(tok string) string {
		ip := net.ParseIP(strings.Trim(tok, ".:"))
		if ip == nil {
			return tok // versions, times, unit names
		}
		if ip.To4() != nil {
			r.removed["IPv4 address"]++
			return strings.Replace(tok, strings.Trim(tok, ".:"), "[ipv4]", 1)
		}
		r.removed["IPv6 address"]++
		return strings.Replace(tok, strings.Trim(tok, ".:"), "[ipv6]", 1)
	})
	for i, re := range r.literals {
		s = r.replace(re, s, r.names[i], "["+r.names[i]+"]")
	}
	return s
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
