package adapter

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetect(t *testing.T) {
	tests := []struct {
		name    string
		release string
		ostree  bool
		bootc   bool
		want    Family
	}{
		{"arch plain", "ID=arch\n", false, false, Arch},
		{"arch quoted", `ID="arch"` + "\n", false, false, Arch},
		{"arch single quoted", "ID='arch'\n", false, false, Arch},
		{"arch via id_like", `ID="manjaro"` + "\n" + `ID_LIKE="arch"` + "\n", false, false, Arch},
		{"arch via id_like middle", `ID="endeavouros"` + "\n" + `ID_LIKE="arch linux"` + "\n", false, false, Arch},
		{"fedora atomic", `ID="fedora"` + "\n", true, false, FedoraAtomic},
		{"fedora via id_like", `ID="bazzite"` + "\n" + `ID_LIKE="fedora"` + "\n", true, false, FedoraAtomic},
		{"fedora bootc", `ID="fedora"` + "\n", false, true, Bootc},
		{"bootc wins over rpm-ostree", `ID="fedora"` + "\n", true, true, Bootc},
		{"fedora without either layer is not atomic", `ID="fedora"` + "\n", false, false, Unknown},
		{"fedora id_like without a layer", `ID="bazzite"` + "\n" + `ID_LIKE="fedora"` + "\n", false, false, Unknown},
		{"debian", `ID=debian` + "\n", false, false, Unknown},
		{"empty", "", false, false, Unknown},
		{"garbage", "this is not os-release\n\x00\x01", false, false, Unknown},
		{"comments and blanks", "# a comment\n\nID=arch\n", false, false, Arch},
		{"arch substring is not arch", `ID="archlinux-extra"` + "\n", false, false, Unknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Detect([]byte(tt.release), tt.ostree, tt.bootc)
			if err != nil {
				t.Fatalf("Detect: %v", err)
			}
			if got != tt.want {
				t.Errorf("Detect = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDefaultsCoverEveryFamily(t *testing.T) {
	table := Defaults()
	for _, family := range []Family{Arch, FedoraAtomic, Bootc} {
		entry, ok := table[family]
		if !ok {
			t.Errorf("no default adapter for %q", family)
			continue
		}
		if len(entry.Stage) == 0 || entry.Stage[0] == "" {
			t.Errorf("%q default stage is empty", family)
		}
		if len(entry.Activate) == 0 || entry.Activate[0] == "" {
			t.Errorf("%q default activate is empty", family)
		}
	}
	if got := strings.Join(table[Arch].Stage, " "); got != "patch-train-update" {
		t.Errorf("arch stage %q", got)
	}
	if got := strings.Join(table[Arch].Activate, " "); got != "omarchy-system reboot" {
		t.Errorf("arch activate %q", got)
	}
}

func TestParseOSRelease(t *testing.T) {
	fields := parseOSRelease([]byte("NAME=\"Arch Linux\"\nID=arch\nID_LIKE=arch\n# c\nbad line\n"))
	if fields["id"] != "arch" {
		t.Errorf("id %q", fields["id"])
	}
	if fields["name"] != "Arch Linux" {
		t.Errorf("name %q", fields["name"])
	}
	if fields["id_like"] != "arch" {
		t.Errorf("id_like %q", fields["id_like"])
	}
	if len(fields) != 3 {
		t.Errorf("got %d fields, want 3: %v", len(fields), fields)
	}
}

const validAdapters = `
[arch]
stage = ["patch-train-update"]
activate = ["omarchy-system", "reboot"]
stamp = "/var/lib/patch-train/version-stamp"

[fedora-atomic]
stage = ["rpm-ostree", "upgrade"]
activate = ["systemctl", "reboot"]
`

func writeAdapters(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "adapters.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoad(t *testing.T) {
	table, err := Load(writeAdapters(t, validAdapters))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(table) != 2 {
		t.Fatalf("got %d sections, want 2", len(table))
	}
	arch, ok := table[Arch]
	if !ok {
		t.Fatal("no arch section")
	}
	if arch.Family != Arch {
		t.Errorf("family %q", arch.Family)
	}
	if strings.Join(arch.Stage, " ") != "patch-train-update" {
		t.Errorf("stage %v", arch.Stage)
	}
	if strings.Join(arch.Activate, " ") != "omarchy-system reboot" {
		t.Errorf("activate %v", arch.Activate)
	}
	if arch.Stamp != "/var/lib/patch-train/version-stamp" {
		t.Errorf("stamp %q", arch.Stamp)
	}
}

func TestLoadRejects(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"empty file", "", "empty"},
		{"whitespace only", "  \n\n", "empty"},
		{"bad toml", "[arch\n", "adapter file"},
		{"unknown section key", "[arch]\nstage=[\"a\"]\nactivate=[\"b\"]\nfamily=\"x\"\n", "unknown key"},
		{"missing stage", "[arch]\nactivate=[\"b\"]\n", "stage"},
		{"empty stage", "[arch]\nstage=[]\nactivate=[\"b\"]\n", "stage"},
		{"empty stage program", "[arch]\nstage=[\"\"]\nactivate=[\"b\"]\n", "stage"},
		{"missing activate", "[arch]\nstage=[\"a\"]\n", "activate"},
		{"empty activate", "[arch]\nstage=[\"a\"]\nactivate=[]\n", "activate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(writeAdapters(t, tt.body))
			if err == nil {
				t.Fatal("Load accepted a bad file")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not name %q", err, tt.want)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err == nil {
		t.Fatal("Load accepted a missing file")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error %q does not match os.ErrNotExist", err)
	}
}

func TestSelect(t *testing.T) {
	table, err := Load(writeAdapters(t, validAdapters))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	got, err := Select(Arch, "", table)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if got.Family != Arch {
		t.Errorf("family %q, want arch", got.Family)
	}

	got, err = Select(FedoraAtomic, "arch", table)
	if err != nil {
		t.Fatalf("Select with override: %v", err)
	}
	if got.Family != Arch {
		t.Errorf("override did not win: family %q", got.Family)
	}

	if _, err := Select(FedoraAtomic, "gentoo", table); err == nil {
		t.Error("Select accepted an override with no section")
	} else if !strings.Contains(err.Error(), "gentoo") {
		t.Errorf("error %q does not name the override", err)
	}

	if _, err := Select(Unknown, "", table); !errors.Is(err, ErrUnknownFamily) {
		t.Errorf("unknown family error %v does not wrap ErrUnknownFamily", err)
	} else if !strings.Contains(err.Error(), "adapter_override") {
		t.Errorf("error %q does not suggest the override", err)
	}

	if _, err := Select(Arch, "", map[Family]Adapter{}); !errors.Is(err, ErrUnknownFamily) {
		t.Errorf("missing section error %v does not wrap ErrUnknownFamily", err)
	}
}
