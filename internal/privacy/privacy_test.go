package privacy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/projectbluefin/bluefin-report/internal/snapshot"
)

func str(v string) json.RawMessage { b, _ := json.Marshal(v); return b }

func metric(name, object, value string) snapshot.Metric {
	return snapshot.Metric{Name: name, Object: object, Value: str(value)}
}

func sample() snapshot.Snapshot {
	return snapshot.Snapshot{
		System: snapshot.Report{MediaType: "application/vnd.io.systemd.report", Metrics: []snapshot.Metric{
			metric("io.systemd.Basic.Hostname", "", "orca-desk"),
			metric("io.systemd.Basic.MachineID", "", "0123456789abcdef0123456789abcdef"),
			metric("io.systemd.Basic.BootID", "", "fedcba9876543210fedcba9876543210"),
			metric("io.systemd.Basic.MachineInfo.LOCATION", "", "Jorge's office"),
			metric("io.systemd.Basic.SMBIOS.SystemSerialNumber", "", "PF3ABCD9"),
			metric("io.systemd.Basic.SMBIOS.SystemUUID", "", "4c4c4544-0042-3510-8051-b4c04f4e5a31"),
			metric("io.systemd.Basic.SMBIOS.BaseBoardSerial", "", "L1HF16M00XY"),
			metric("io.systemd.Basic.SMBIOS.ChassisSerialNumber", "", "CHS99887766"),
			metric("io.systemd.Basic.SMBIOS.BaseBoardAssetTag", "", "ASSET-0042"),
			metric("io.systemd.Basic.SMBIOS.ChassisAssetTagNumber", "", "TAG-7788"),
			metric("io.systemd.Basic.SMBIOS.SystemProductName", "", "Laptop 13 (AMD Ryzen 7040Series)"),
			metric("io.systemd.Report.SignPlain.PublicKey", "", "-----BEGIN PUBLIC KEY-----\nMCowBQYDK2VwAyEAuniquekey\n-----END PUBLIC KEY-----"),
			metric("io.systemd.Basic.KernelVersion", "", "7.2.11-ogc4"),
			metric("io.systemd.Basic.OSRelease.IMAGE_ID", "", "dakota-gaming"),
			metric("io.systemd.Files.os-release", "", "ID=bluefin"),
			metric("io.systemd.Manager.UnitActiveState", "var-home-jorge-vault.mount", "failed"),
			metric("io.systemd.Manager.UnitActiveState", "orca-desk-backup.service", "failed"),
			metric("io.systemd.Manager.UnitActiveState", "rpc-statd.service", "failed"),
			metric("io.systemd.Manager.UnitActiveState", "getty@tty1.service", "active"),
			metric("io.systemd.Manager.StateChangeTimestamp", "sshd.service", "00:37:42"),
			{Name: "io.systemd.Network.Address", Object: "wlp1s0", Value: str("192.168.7.21"),
				Fields: map[string]any{"mac": "3c:22:fb:01:aa:9e", "v6": "fe80::3e22:fbff:fe01:aa9e", "n": 1.0}},
			metric("io.systemd.Network.Owner", "", "contact jorge@example.org about /var/home/jorge/notes"),
		}},
		User: snapshot.Report{Metrics: []snapshot.Metric{
			metric("io.systemd.Manager.UnitActiveState", "app-orca\\x2ddesk.scope", "active"),
		}},
	}
}

func TestBuildRemovesIdentifiersAndKeepsDiagnostics(t *testing.T) {
	p, err := Build(sample(), Options{Username: "jorge", IncludeUser: true})
	if err != nil {
		t.Fatal(err)
	}
	out := string(p.JSON)
	for _, secret := range []string{"orca-desk", "x2ddesk", "0123456789abcdef", "fedcba98", "Jorge's office", "jorge",
		"192.168.7.21", "3c:22:fb", "fe80::", "example.org", "ID=bluefin",
		"PF3ABCD9", "4c4c4544", "L1HF16M00XY", "CHS99887766", "ASSET-0042", "TAG-7788", "uniquekey"} {
		if strings.Contains(out, secret) {
			t.Errorf("payload still contains %q", secret)
		}
	}
	for _, keep := range []string{"7.2.11-ogc4", "dakota-gaming", "Laptop 13 (AMD Ryzen 7040Series)", "rpc-statd.service", "getty@tty1.service", "00:37:42", "wlp1s0", "var-home-[username]-vault.mount"} {
		if !strings.Contains(out, keep) {
			t.Errorf("payload lost diagnostic value %q", keep)
		}
	}
	var doc map[string]any
	if err := json.Unmarshal(p.JSON, &doc); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if doc["user"] == nil {
		t.Error("user scope requested but missing")
	}
	if p.Removed["identifying metric"] != 12 || p.Removed["MAC address"] != 1 || p.Removed["IPv4 address"] != 1 ||
		p.Removed["IPv6 address"] != 1 || p.Removed["email"] != 1 || p.Removed["home path"] != 1 {
		t.Errorf("Removed = %v", p.Removed)
	}
}

func TestBuildUserScopeAndFilesAreOptIn(t *testing.T) {
	p, err := Build(sample(), Options{Username: "jorge", KeepFiles: []string{"io.systemd.Files.os-release"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(p.JSON), `"user"`) {
		t.Error("user scope attached without opt-in")
	}
	if !strings.Contains(string(p.JSON), "ID=bluefin") {
		t.Error("opted-in file family was dropped")
	}
}

func TestBuildBudgetOmitsWholeFamiliesAndStaysValid(t *testing.T) {
	full, _ := Build(sample(), Options{Username: "jorge"})
	limit := len(full.JSON) - 50

	p, err := Build(sample(), Options{Username: "jorge", MaxBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.JSON) > limit {
		t.Errorf("payload %d bytes exceeds budget %d", len(p.JSON), limit)
	}
	var doc struct {
		System          snapshot.Report
		OmittedFamilies []string
	}
	if err := json.Unmarshal(p.JSON, &doc); err != nil {
		t.Fatalf("payload is not valid JSON: %v", err)
	}
	if len(p.Omitted) == 0 || p.Omitted[0] != "io.systemd.Manager.UnitActiveState" || doc.OmittedFamilies[0] != p.Omitted[0] {
		t.Errorf("Omitted = %v (largest family should go first, and be recorded in the document)", p.Omitted)
	}
	for _, m := range doc.System.Metrics {
		if m.Name == p.Omitted[0] {
			t.Error("omitted family still partially present")
		}
	}

	if _, err := Build(sample(), Options{MaxBytes: 10}); err == nil {
		t.Error("expected error when nothing fits")
	}
}
