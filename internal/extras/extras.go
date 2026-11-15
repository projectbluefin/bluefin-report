// Package extras collects what systemd-report does not: image identity for the Overview,
// and opt-in diagnostic sections (journal excerpts, device state, disk and GPU readings).
// Every probe runs as the logged-in user, is read-only, and tolerates failure: a missing
// tool or file yields "" rather than an error.
package extras

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/projectbluefin/bluefin-report/internal/i18n"
)

// Identity is shown in the Overview and the issue summary table.
type Identity struct{ Ref, Tag, Digest, CPU, GPU, GNOME string }

// Root prefixes every file read and absolute command; tests and screenshot
// captures point it at fixtures.
var Root = "/"

// run is replaced in tests; output is capped at 64 KiB.
var run = func(ctx context.Context, name string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if filepath.IsAbs(name) {
		name = filepath.Join(Root, name)
	}
	out, _ := exec.CommandContext(ctx, name, args...).Output()
	if len(out) > 64<<10 {
		out = out[len(out)-64<<10:] // keep the newest lines
	}
	return strings.TrimSpace(string(out))
}

func read(p string) string {
	b, _ := os.ReadFile(filepath.Join(Root, p))
	return string(b)
}

// Probe collects the identity. Call it off the main thread.
func Probe(ctx context.Context) Identity {
	id := deployID(read("proc/cmdline"))
	return Identity{
		Ref:    run(ctx, "/usr/libexec/ublue-booted-image", "ref"),
		Tag:    run(ctx, "/usr/libexec/ublue-booted-image", "tag"),
		Digest: originDigest(read(filepath.Join("sysroot/state/deploy", id, id+".origin"))),
		CPU:    cpuModel(read("proc/cpuinfo")),
		GPU:    gpus(),
		GNOME:  strings.TrimPrefix(run(ctx, "gnome-shell", "--version"), "GNOME Shell "),
	}
}

var reDigest = regexp.MustCompile(`(?:^|\s)composefs\.digest=(?:[^:\s]*:)?([0-9a-f]+)(?:\s|$)`)

// deployID returns the booted composefs deployment id from /proc/cmdline, or "".
func deployID(cmdline string) string {
	if m := reDigest.FindStringSubmatch(cmdline); m != nil {
		return m[1]
	}
	return ""
}

// originDigest returns [image] manifest_digest from a deployment .origin file.
func originDigest(origin string) string {
	section := ""
	for _, l := range strings.Split(origin, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") {
			section = l
		} else if k, v, ok := strings.Cut(l, "="); ok && section == "[image]" && strings.TrimSpace(k) == "manifest_digest" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func cpuModel(cpuinfo string) string {
	for _, l := range strings.Split(cpuinfo, "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok && strings.TrimSpace(k) == "model name" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

var vendors = map[string]string{"1002": "AMD", "10DE": "NVIDIA", "8086": "Intel"}

// gpu formats one DRM card's uevent as "AMD 1002:744C (amdgpu)".
func gpu(uevent string) string {
	var id, drv string
	for _, l := range strings.Split(uevent, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), "=")
		switch k {
		case "PCI_ID":
			id = strings.ToUpper(v)
		case "DRIVER":
			drv = v
		}
	}
	if id == "" {
		return ""
	}
	s := id
	if v, ok := vendors[id[:strings.Index(id+":", ":")]]; ok {
		s = v + " " + id
	}
	if drv != "" {
		s += " (" + drv + ")"
	}
	return s
}

var reCard = regexp.MustCompile(`^card[0-9]+$`) // not connectors like card0-DP-1

func gpus() string {
	ents, _ := os.ReadDir(filepath.Join(Root, "sys/class/drm"))
	var out []string
	for _, e := range ents {
		if reCard.MatchString(e.Name()) {
			if g := gpu(read(filepath.Join("sys/class/drm", e.Name(), "device/uevent"))); g != "" {
				out = append(out, g)
			}
		}
	}
	return strings.Join(out, ", ")
}

// Section is one opt-in block. Details marks those also shown, collapsed, in the issue body.
// Title is English, as the issue body needs; the UI shows it through i18n.G.
type Section struct {
	Key, Title string
	Details    bool
	collect    func(context.Context) string
}

// part is one source of a section: a header line naming it, and its output.
type part struct {
	head string
	get  func(context.Context) string
}

func sh(name string, args ...string) part {
	return part{"$ " + name + " " + strings.Join(args, " "), func(ctx context.Context) string { return run(ctx, name, args...) }}
}

// join renders the non-empty parts under their headers, a blank line between; a
// missing tool contributes nothing.
func join(ps ...part) func(context.Context) string {
	return func(ctx context.Context) string {
		var out []string
		for _, p := range ps {
			if v := p.get(ctx); v != "" {
				out = append(out, p.head+"\n"+v)
			}
		}
		return strings.Join(out, "\n\n")
	}
}

// warnings is the newest n warning-or-worse journal entries of this boot matching match.
func warnings(n string, match ...string) part {
	return sh("journalctl", append([]string{"-b0", "-p", "warning", "-n", n, "--no-pager", "-o", "short", "--no-hostname"}, match...)...)
}

var Sections = []Section{
	{"bootErrors", i18n.N("Errors From This Boot"), true, sh("journalctl", "-b0", "-p", "err", "--no-pager", "-n", "40", "-o", "short", "--no-hostname").get},
	{"crashes", i18n.N("Crashes in the Last 7 Days"), true, sh("coredumpctl", "list", "--no-pager", "--since", "-7d").get},
	// No -n: with --grep it makes journalctl print newest-first. One boot's kernel
	// log is small, and run keeps only the newest 64 KiB anyway.
	{"previousBoot", i18n.N("Kernel Problems in the Previous Boot"), false, sh("journalctl", "-b", "-1", "-k", "--no-pager", "-o", "short", "--no-hostname",
		"--case-sensitive=false", "--grep", "panic|oops|bug:|call trace|suspend|hibernate|resume|hung task|lockup").get},
	{"desktopLog", i18n.N("Desktop Warnings"), false, warnings("200", "_SYSTEMD_UNIT=gdm.service", "+", "_COMM=gnome-shell").get},
	{"updateLog", i18n.N("Update and Boot Warnings"), false, warnings("250", "-u", "bootc-fetch-apply-updates.service", "-u", "rpm-ostreed.service", "-u", "systemd-boot-update.service").get},
	{"networkLog", i18n.N("Network Warnings and Device State"), false, join(
		warnings("250", "-u", "NetworkManager.service"),           // quoted connection names: removed by privacy
		sh("nmcli", "-t", "-f", "TYPE,STATE", "device", "status"), // no DEVICE/CONNECTION: interface and SSID names identify networks
	)},
	// Portals run as user units; the glob also catches backends (-gnome, -gtk).
	{"portalLog", i18n.N("Flatpak and Portal Warnings"), false, warnings("250", "-u", "flatpak-system-helper.service", "--user-unit", "xdg-desktop-portal*").get},
	{"hardware", i18n.N("Disks and Graphics"), false, join(
		sh("lsblk", "-d", "-e", "7,11", "-o", "KNAME,SIZE,ROTA,TRAN,MODEL"), // KNAME: mapper aliases can be WWIDs; never SERIAL/WWN
		sh("nvidia-smi", "--query-gpu=name,driver_version,pstate,temperature.gpu,utilization.gpu,memory.used,memory.total,power.draw", "--format=csv"),
		part{"# amdgpu: /sys/class/drm/card*/device", func(context.Context) string { return amdgpus() }},
	)},
	{"gnomeExtensions", i18n.N("Enabled GNOME Extensions"), false, sh("gnome-extensions", "list", "--enabled").get},
	{"flatpaks", i18n.N("Installed Flatpak Apps"), false, sh("flatpak", "list", "--app", "--columns=application,version").get},
}

func num(p string) (float64, bool) {
	v, err := strconv.ParseFloat(strings.TrimSpace(read(p)), 64)
	return v, err == nil
}

// amdgpus reports load, memory, temperature and power of each amdgpu card, one line each;
// values the driver does not expose are left out.
func amdgpus() string {
	ents, _ := os.ReadDir(filepath.Join(Root, "sys/class/drm"))
	var out []string
	for _, e := range ents {
		if !reCard.MatchString(e.Name()) {
			continue
		}
		dev := filepath.Join("sys/class/drm", e.Name(), "device")
		busy, ok := num(filepath.Join(dev, "gpu_busy_percent"))
		if !ok {
			continue
		}
		f := []string{fmt.Sprintf("busy %.0f%%", busy)}
		for _, m := range []string{"vram", "gtt"} {
			u, ok1 := num(filepath.Join(dev, "mem_info_"+m+"_used"))
			t, ok2 := num(filepath.Join(dev, "mem_info_"+m+"_total"))
			if ok1 && ok2 {
				f = append(f, fmt.Sprintf("%s %.1f GiB / %.1f GiB", strings.ToUpper(m), u/(1<<30), t/(1<<30)))
			}
		}
		if hw, _ := filepath.Glob(filepath.Join(Root, dev, "hwmon/hwmon*")); len(hw) > 0 {
			h, _ := filepath.Rel(Root, hw[0])
			if v, ok := num(filepath.Join(h, "temp1_input")); ok {
				f = append(f, fmt.Sprintf("temp %.0f°C", v/1000))
			}
			v, ok := num(filepath.Join(h, "power1_average"))
			if !ok {
				v, ok = num(filepath.Join(h, "power1_input")) // APUs
			}
			if ok {
				f = append(f, fmt.Sprintf("power %.0f W", v/1e6))
			}
		}
		out = append(out, e.Name()+": "+strings.Join(f, ", "))
	}
	return strings.Join(out, "\n")
}

// MaxSection caps each section so that ten of them stay under the 1 MiB attachment even
// if every byte is JSON-escaped to six (`<` → \u003c): 10 × 16 KiB × 6 < privacy.MaxBytes.
const MaxSection = 16 << 10

// Collect runs only the sections whose keys are given. Call it off the main thread.
func Collect(ctx context.Context, keys []string) map[string]string {
	out := map[string]string{}
	for _, s := range Sections {
		for _, k := range keys {
			if k == s.Key {
				v := s.collect(ctx)
				if len(v) > MaxSection { // keep the newest whole lines
					v = v[len(v)-MaxSection:]
					v = v[strings.IndexByte(v, '\n')+1:]
				}
				out[k] = v
			}
		}
	}
	return out
}

// Markdown renders the Details sections present in vals as collapsed blocks.
func Markdown(vals map[string]string) string {
	var b strings.Builder
	for _, s := range Sections {
		v, ok := vals[s.Key]
		if !ok || !s.Details {
			continue
		}
		if v == "" {
			v = "(none)"
		}
		fmt.Fprintf(&b, "\n\n<details><summary>%s</summary>\n\n```\n%s\n```\n\n</details>", s.Title, strings.ReplaceAll(v, "```", "'''"))
	}
	return b.String()
}
