package extras

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const id = "22791cb751e37b3ee143a287d1922f27"

func fixture(t *testing.T) {
	t.Helper()
	Root = t.TempDir()
	t.Cleanup(func() { Root = "/" })
	for p, c := range map[string]string{
		"proc/cmdline": `initrd=\EFI\Linux\bootc_composefs-ab\initrd root=LABEL=root rw quiet composefs.digest=v1-sha512-12:` + id + "\n",
		"proc/cpuinfo": "processor\t: 0\nvendor_id\t: AuthenticAMD\nmodel name\t: AMD Ryzen 5 7600X 6-Core Processor\n",
		"sysroot/state/deploy/" + id + "/" + id + ".origin": "[origin]\ncontainer-image-reference = docker://ghcr.io/projectbluefin/dakota:testing\n\n" +
			"[boot]\ndigest = ae77\n\n[image]\nmanifest_digest = sha256:4a06d9\n",
		"sysroot/state/deploy/other/other.origin": "[image]\nmanifest_digest = sha256:wrong\n",
		"sys/class/drm/card0/device/uevent":       "DRIVER=amdgpu\nPCI_CLASS=30000\nPCI_ID=1002:164E\n",
		"sys/class/drm/card1/device/uevent":       "DRIVER=nvidia\nPCI_ID=10de:2684\n",
		"sys/class/drm/card0-DP-1/device/uevent":  "DRIVER=amdgpu\nPCI_ID=1002:164E\n",
		"sys/class/drm/renderD128/device/uevent":  "PCI_ID=1002:164E\n",
	} {
		p = filepath.Join(Root, p)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func stub(t *testing.T, out map[string]string) *[]string {
	var calls []string
	old := run
	run = func(_ context.Context, name string, args ...string) string {
		k := strings.Join(append([]string{name}, args...), " ")
		calls = append(calls, k)
		return out[k]
	}
	t.Cleanup(func() { run = old })
	return &calls
}

func TestProbe(t *testing.T) {
	fixture(t)
	stub(t, map[string]string{
		"/usr/libexec/ublue-booted-image ref": "ghcr.io/projectbluefin/dakota:testing",
		"/usr/libexec/ublue-booted-image tag": "testing",
		"gnome-shell --version":               "GNOME Shell 51.1",
	})
	want := Identity{Ref: "ghcr.io/projectbluefin/dakota:testing", Tag: "testing", Digest: "sha256:4a06d9",
		CPU: "AMD Ryzen 5 7600X 6-Core Processor", GPU: "AMD 1002:164E (amdgpu), NVIDIA 10DE:2684 (nvidia)", GNOME: "51.1"}
	if got := Probe(context.Background()); got != want {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestProbeToleratesMissingSources(t *testing.T) {
	Root = t.TempDir()
	t.Cleanup(func() { Root = "/" })
	stub(t, nil)
	if got := Probe(context.Background()); got != (Identity{}) {
		t.Fatalf("got %+v", got)
	}
}

// The screenshot fixture tree must stand in for the host entirely, commands included.
func TestProbeScreenshotFixtureRoot(t *testing.T) {
	root, err := filepath.Abs("testdata/root")
	if err != nil {
		t.Fatal(err)
	}
	Root = root
	t.Cleanup(func() { Root = "/" })
	t.Setenv("PATH", filepath.Join(root, "usr/bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	want := Identity{Ref: "ghcr.io/projectbluefin/dakota-gaming:20261031", Tag: "20261031", Digest: "sha256:0000fixture",
		CPU: "AMD Ryzen 7 7840U w/ Radeon 780M Graphics", GPU: "AMD 1002:15BF (amdgpu)", GNOME: "49.1"}
	if got := Probe(context.Background()); got != want {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

func TestCollectOnlyOptedIn(t *testing.T) {
	calls := stub(t, map[string]string{"coredumpctl list --no-pager --since -7d": "TIME PID\nx 1 /usr/bin/foo"})
	if got := Collect(context.Background(), nil); len(got) != 0 || len(*calls) != 0 {
		t.Fatalf("nothing opted in, yet collected %v via %v", got, *calls)
	}
	got := Collect(context.Background(), []string{"crashes", "flatpaks"})
	if len(*calls) != 2 || got["crashes"] == "" || len(got) != 2 {
		t.Fatalf("%v %v", got, *calls)
	}
	md := Markdown(got)
	if !strings.Contains(md, "<details><summary>Crashes in the Last 7 Days</summary>") || strings.Contains(md, "Flatpak") {
		t.Fatalf("markdown: %s", md)
	}
}

// Diagnostic output lands verbatim inside a code fence in a public issue body, so a
// line of backticks in it must not close the fence, and an opted-in section that
// printed nothing must still say so instead of rendering an empty block.
func TestMarkdownEscapesFencesAndMarksEmpty(t *testing.T) {
	md := Markdown(map[string]string{
		"crashes":    "a\n```\n# injected heading\n```",
		"bootErrors": "",
		"flatpaks":   "org.example.App 1.0",
	})
	want := "\n\n<details><summary>Errors From This Boot</summary>\n\n```\n(none)\n```\n\n</details>" +
		"\n\n<details><summary>Crashes in the Last 7 Days</summary>\n\n```\na\n'''\n# injected heading\n'''\n```\n\n</details>"
	if md != want {
		t.Fatalf("markdown:\n%q\nwant:\n%q", md, want)
	}
}

// byName stubs run by program name only, so tests don't pin flags.
func byName(t *testing.T, out map[string]string) {
	old := run
	run = func(_ context.Context, name string, _ ...string) string { return out[name] }
	t.Cleanup(func() { run = old })
}

func amdCard(t *testing.T, card string, files map[string]string) {
	t.Helper()
	for p, c := range files {
		p = filepath.Join(Root, "sys/class/drm", card, "device", p)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(c+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Each part of a multi-part section sits under a header naming its source; a tool
// that is missing (or printed nothing) leaves no orphaned header behind.
func TestCollectJoinsPartsOmittingMissingTools(t *testing.T) {
	Root = t.TempDir()
	t.Cleanup(func() { Root = "/" })
	byName(t, map[string]string{"journalctl": "j1\nj2", "lsblk": "NAME SIZE\nsda 1T"})
	got := Collect(context.Background(), []string{"networkLog", "hardware"})
	for key, bodies := range map[string][]string{"networkLog": {"j1\nj2"}, "hardware": {"NAME SIZE\nsda 1T"}} {
		ps := strings.Split(got[key], "\n\n")
		if len(ps) != len(bodies) {
			t.Fatalf("%s: want %d parts, got %q", key, len(bodies), got[key])
		}
		for i, p := range ps {
			head, body, _ := strings.Cut(p, "\n")
			if !strings.HasPrefix(head, "$ ") || body != bodies[i] {
				t.Fatalf("%s part %d: %q", key, i, p)
			}
		}
	}
	byName(t, nil)
	if got := Collect(context.Background(), []string{"hardware"}); got["hardware"] != "" {
		t.Fatalf("nothing available, got %q", got["hardware"])
	}
}

// Ten sections share the 1 MiB attachment, so each is capped, keeping its newest
// whole lines; otherwise opting into everything could leave no room for the report.
func TestCollectCapsEachSection(t *testing.T) {
	Root = t.TempDir()
	t.Cleanup(func() { Root = "/" })
	line := strings.Repeat("x", 99) + "\n"
	byName(t, map[string]string{"lsblk": "oldest\n" + strings.Repeat(line, 1000) + "newest", "journalctl": "small"})
	got := Collect(context.Background(), []string{"hardware", "bootErrors"})
	h := got["hardware"]
	if len(h) > MaxSection || !strings.HasSuffix(h, "newest") || strings.Contains(h, "oldest") || !strings.HasPrefix(h, "x") {
		t.Fatalf("hardware: %d bytes, starts %q", len(h), h[:min(len(h), 20)])
	}
	if got["bootErrors"] != "small" {
		t.Fatalf("small section changed: %q", got["bootErrors"])
	}
}

func TestCollectAMDSysfs(t *testing.T) {
	Root = t.TempDir()
	t.Cleanup(func() { Root = "/" })
	byName(t, nil)
	amdCard(t, "card1", map[string]string{
		"gpu_busy_percent": "6", "mem_info_vram_used": "1432150016", "mem_info_vram_total": "25753026560",
		"mem_info_gtt_used": "82116608", "mem_info_gtt_total": "16369799168",
		"hwmon/hwmon4/temp1_input": "46000", "hwmon/hwmon4/power1_average": "30000000",
	})
	// An APU: power1_input instead of power1_average, no GTT total.
	amdCard(t, "card0", map[string]string{
		"gpu_busy_percent": "0", "mem_info_vram_used": "536870912", "mem_info_vram_total": "2147483648",
		"mem_info_gtt_used": "1", "hwmon/hwmon3/power1_input": "57647000",
	})
	amdCard(t, "card2", map[string]string{"gpu_busy_percent": "100"}) // no memory, no hwmon
	amdCard(t, "card3", map[string]string{"uevent": "DRIVER=nvidia"}) // not amdgpu
	amdCard(t, "card1-DP-1", map[string]string{"gpu_busy_percent": "1"})
	_, body, _ := strings.Cut(Collect(context.Background(), []string{"hardware"})["hardware"], "\n")
	want := "card0: busy 0%, VRAM 0.5 GiB / 2.0 GiB, power 58 W\n" +
		"card1: busy 6%, VRAM 1.3 GiB / 24.0 GiB, GTT 0.1 GiB / 15.2 GiB, temp 46°C, power 30 W\n" +
		"card2: busy 100%"
	if body != want {
		t.Fatalf("got\n%s\nwant\n%s", body, want)
	}
}
