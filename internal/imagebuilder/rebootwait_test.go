package imagebuilder

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Shaped like image-builder's real packer.json: the reboot, then the Ansible
// run that upstream gives only 10s to reconnect.
const packerFixture = `{
  "builders": [{"type": "proxmox-iso", "ssh_timeout": "2h"}],
  "provisioners": [
    {"type": "shell", "inline": ["echo first"]},
    {"type": "ansible", "playbook_file": "./ansible/firstboot.yml"},
    {"type": "shell", "expect_disconnect": true, "inline": ["sudo reboot now"]},
    {"type": "ansible", "playbook_file": "./ansible/node.yml", "pause_before": "10s"},
    {"type": "goss"}
  ]
}`

func provisioners(t *testing.T, raw []byte) []map[string]any {
	t.Helper()
	var doc struct {
		Provisioners []map[string]any `json:"provisioners"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	return doc.Provisioners
}

func TestPatchRebootWaitInsertsBeforeNodeAnsible(t *testing.T) {
	out, patched, err := patchRebootWait([]byte(packerFixture))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !patched {
		t.Fatal("expected the fixture to be patched")
	}
	provs := provisioners(t, out)
	if len(provs) != 6 {
		t.Fatalf("expected 6 provisioners after insertion, got %d", len(provs))
	}
	// The wait must land AFTER the reboot and IMMEDIATELY BEFORE node.yml —
	// anywhere else and it does not close the race it exists for.
	if provs[2]["type"] != "shell" || provs[2]["expect_disconnect"] != true {
		t.Fatalf("reboot provisioner moved: %v", provs[2])
	}
	inline, _ := json.Marshal(provs[3]["inline"])
	if provs[3]["type"] != "shell" || !strings.Contains(string(inline), rebootWaitMarker) {
		t.Fatalf("inserted provisioner is not at index 3: %v", provs[3])
	}
	if provs[4]["playbook_file"] != "./ansible/node.yml" {
		t.Fatalf("node.yml is not immediately after the wait: %v", provs[4])
	}
}

// Re-rendering happens on every build, so a second pass must not stack a
// duplicate provisioner.
func TestPatchRebootWaitIsIdempotent(t *testing.T) {
	once, _, err := patchRebootWait([]byte(packerFixture))
	if err != nil {
		t.Fatal(err)
	}
	twice, patched, err := patchRebootWait(once)
	if err != nil {
		t.Fatal(err)
	}
	if patched {
		t.Fatal("second pass reported patching an already-patched file")
	}
	if len(provisioners(t, twice)) != len(provisioners(t, once)) {
		t.Fatal("second pass changed the provisioner count")
	}
}

// Upstream drift must degrade to "leave it alone", never to a wrong guess.
func TestPatchRebootWaitLeavesUnknownShapesAlone(t *testing.T) {
	cases := map[string]string{
		"no provisioners key": `{"builders": []}`,
		"no node.yml":         `{"provisioners": [{"type": "shell"}, {"type": "goss"}]}`,
		"node.yml is first":   `{"provisioners": [{"type": "ansible", "playbook_file": "./ansible/node.yml"}]}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			out, patched, err := patchRebootWait([]byte(in))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if patched {
				t.Fatal("should not have patched an unrecognised shape")
			}
			if string(out) != in {
				t.Fatalf("input was modified:\n%s", out)
			}
		})
	}
}

func TestPatchRebootWaitRejectsInvalidJSON(t *testing.T) {
	if _, _, err := patchRebootWait([]byte("{not json")); err == nil {
		t.Fatal("expected an error for malformed input")
	}
}

// Untouched provisioners must survive byte-for-byte — carrying them as
// json.RawMessage is the whole reason the patch re-marshals only the list.
func TestPatchRebootWaitPreservesOtherProvisioners(t *testing.T) {
	out, _, err := patchRebootWait([]byte(packerFixture))
	if err != nil {
		t.Fatal(err)
	}
	provs := provisioners(t, out)
	if provs[1]["playbook_file"] != "./ansible/firstboot.yml" {
		t.Fatalf("firstboot provisioner altered: %v", provs[1])
	}
	if provs[5]["type"] != "goss" {
		t.Fatalf("goss provisioner altered: %v", provs[5])
	}
	// The builder block must be carried through untouched.
	var doc struct {
		Builders []map[string]any `json:"builders"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Builders) != 1 || doc.Builders[0]["ssh_timeout"] != "2h" {
		t.Fatalf("builders block altered: %v", doc.Builders)
	}
}

// The render step failed in production with permission denied because the
// existing packer.json was owned by the build container's UID and PVEKube
// tried to write it in place. Replacing an unwritable file is therefore the
// case that actually matters, not just writing a fresh one.
func TestWriteFileAtomicReplacesUnwritableFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "packer.json")
	if err := os.WriteFile(dst, []byte("stale"), 0o444); err != nil {
		t.Fatal(err)
	}
	// 0444 with no owner-write is the closest a test running as the file's
	// owner can get to "another UID owns this"; a direct write must fail.
	if err := os.WriteFile(dst, []byte("direct"), 0o444); err == nil {
		t.Skip("this platform allows the owner to write a 0444 file; the regression cannot be reproduced here")
	}

	if err := writeFileAtomic(filepath.Join(dir, "packer.json"), []byte("fresh")); err != nil {
		t.Fatalf("writeFileAtomic could not replace an unwritable file: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "fresh" {
		t.Fatalf("content = %q, want fresh", got)
	}
	// The container reads this as a different UID, so it must not come out 0600.
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o044 == 0 {
		t.Fatalf("mode %v is not readable by other users", info.Mode().Perm())
	}
}

// No temp files may be left behind in the packer directory — Packer globs
// that directory and a stray .packer.json-* would be confusing at best.
func TestWriteFileAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	if err := writeFileAtomic(filepath.Join(dir, "packer.json"), []byte("x")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".pvekube-tmp-") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly packer.json, got %d entries", len(entries))
	}
}

// The build VM's memory override must actually reach Packer — image-builder's
// 2048 default is what the installer crashed under.
func TestPackerFlagsOverridesBuildVMMemory(t *testing.T) {
	got := packerFlagsEnv(ConnEnv{DiskFormat: "raw"}, 119, KubernetesVersion{})
	if len(got) != 1 || !strings.HasPrefix(got[0], "PACKER_FLAGS=") {
		t.Fatalf("unexpected env shape: %v", got)
	}
	if !strings.Contains(got[0], "--var memory=4096") {
		t.Fatalf("memory override missing from PACKER_FLAGS: %q", got[0])
	}
	// The pre-existing flags must survive alongside it.
	for _, want := range []string{"--var disk_format=raw", "--var vmid=119", "extra_debs"} {
		if !strings.Contains(got[0], want) {
			t.Fatalf("PACKER_FLAGS lost %q: %s", want, got[0])
		}
	}
}
