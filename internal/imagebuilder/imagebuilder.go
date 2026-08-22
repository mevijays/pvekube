// Package imagebuilder drives kubernetes-sigs/image-builder to turn a
// Proxmox node into a Kubernetes-ready VM template, without the operator
// ever touching Packer, Ansible, or a shell.
//
// The container image (versions.ImageBuilderImage) bundles the toolchain —
// Packer, Ansible, goss — but NOT the actual build definitions (Makefile,
// per-OS Packer configs, cloud-init templates). Those come from a shallow
// clone of the image-builder repo at a pinned tag, bind-mounted into the
// container at build time. This mirrors exactly how upstream CI and the
// project's own docs use the container — PVEKube automates the same steps
// a human would type, it doesn't reimplement Packer's job.
package imagebuilder

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"pvekube/internal/jobs"
	"pvekube/internal/proxmox"
	"pvekube/internal/runner"
	"pvekube/internal/versions"
)

// OSFlavor is one buildable target, matching an image-builder Makefile
// suffix exactly (build-proxmox-<ID> / validate-proxmox-<ID>).
type OSFlavor struct {
	ID    string
	Label string
}

// Flavors is the supported set, taken directly from image-builder's
// PROXMOX_BUILD_NAMES (kubernetes-sigs/image-builder images/capi/Makefile).
var Flavors = []OSFlavor{
	{ID: "ubuntu-2204", Label: "Ubuntu 22.04"},
	{ID: "ubuntu-2404", Label: "Ubuntu 24.04"},
	{ID: "ubuntu-2404-efi", Label: "Ubuntu 24.04 (EFI boot)"},
	{ID: "ubuntu-2604", Label: "Ubuntu 26.04"},
	{ID: "ubuntu-2604-efi", Label: "Ubuntu 26.04 (EFI boot)"},
	{ID: "rockylinux-9", Label: "Rocky Linux 9"},
	{ID: "flatcar", Label: "Flatcar Container Linux"},
}

// RepoDir is where the pinned image-builder checkout lives.
func RepoDir(dataDir string) string {
	return filepath.Join(dataDir, "image-builder")
}

// ConnEnv is the subset of a Proxmox connection image-builder's Packer
// config needs, resolved from a stored connection + discovery snapshot
// rather than asked of the user again — everything here was already
// gathered on the Proxmox connection screen.
type ConnEnv struct {
	URL         string // https://host:8006 (without /api2/json — added below)
	TokenID     string // capmox@pve!capi — packer's "PROXMOX_USERNAME" despite the name
	Secret      string
	InsecureTLS bool
	Node        string
	ISOPool     string
	Bridge      string
	StoragePool string
	// DiskFormat is "qcow2" or "raw", resolved from proxmox.Storage.DiskFormat
	// for StoragePool — LVM-thin and ZFS pools reject qcow2 outright
	// ("unsupported format 'qcow2'"), and image-builder's Packer template
	// defaults to qcow2 unconditionally, so this must be threaded through
	// explicitly rather than left to that default. Empty falls back to
	// "qcow2" (image-builder's own default) for backwards compatibility.
	DiskFormat string
}

func (e ConnEnv) proxmoxClient() *proxmox.Client {
	return proxmox.New(proxmox.Config{
		URL: e.URL, TokenID: e.TokenID, Secret: e.Secret, InsecureSkipVerify: e.InsecureTLS,
	})
}

func (e ConnEnv) dockerEnvArgs() []string {
	return []string{
		"-e", "PROXMOX_URL=" + e.URL + "/api2/json",
		"-e", "PROXMOX_USERNAME=" + e.TokenID,
		"-e", "PROXMOX_TOKEN=" + e.Secret,
		"-e", "PROXMOX_NODE=" + e.Node,
		"-e", "PROXMOX_ISO_POOL=" + e.ISOPool,
		"-e", "PROXMOX_BRIDGE=" + e.Bridge,
		"-e", "PROXMOX_STORAGE_POOL=" + e.StoragePool,
	}
}

// EnsureRepoStep clones the pinned image-builder tag if not already present.
// Idempotent — safe to run before every build/validate.
func EnsureRepoStep(dataDir string) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		dir := RepoDir(dataDir)
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			c.Logf("image-builder repo already present at %s", dir)
			return nil
		}
		c.Logf("Cloning kubernetes-sigs/image-builder@%s (one-time, ~few seconds)", versions.ImageBuilderRepoRef)
		return runner.Run(c, c, "", nil, "git", "clone", "--depth", "1",
			"--branch", versions.ImageBuilderRepoRef,
			"https://github.com/kubernetes-sigs/image-builder.git", dir)
	}
}

// isoSpec is the subset of a per-flavor Packer JSON config (e.g.
// packer/proxmox/ubuntu-2204.json) needed to pre-stage that flavor's
// installer ISO directly on Proxmox.
type isoSpec struct {
	URL          string `json:"iso_url"`
	Checksum     string `json:"iso_checksum"`
	ChecksumType string `json:"iso_checksum_type"`
	// IsoFile is the raw "{{env `ISO_FILE`}}" placeholder string when the
	// flavor supports pointing at a pre-staged ISO; empty for flavors
	// (flatcar, rockylinux) whose upstream config doesn't wire this up.
	IsoFile string `json:"iso_file"`
}

// DefaultKubernetesSemver reads image-builder's own pinned default Kubernetes
// version (packer/config/kubernetes.json's "kubernetes_semver", e.g.
// "v1.36.1") for when a build leaves the Kubernetes version field blank.
// Callers must not use this as a placeholder string ("image-builder
// default") instead — that string isn't a semver and clusterctl rejects it
// outright ("invalid KubernetesVersion. Please use a semantic version
// number") the moment someone tries to generate a cluster from that
// template. Requires the image-builder repo to already be cloned.
func DefaultKubernetesSemver(dataDir string) (string, error) {
	path := filepath.Join(RepoDir(dataDir), "images", "capi", "packer", "config", "kubernetes.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", path, err)
	}
	var cfg struct {
		KubernetesSemver string `json:"kubernetes_semver"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "", fmt.Errorf("parsing %s: %w", path, err)
	}
	if cfg.KubernetesSemver == "" {
		return "", fmt.Errorf("%s has no kubernetes_semver", path)
	}
	return cfg.KubernetesSemver, nil
}

func readISOSpec(dataDir string, flavor OSFlavor) (isoSpec, error) {
	path := filepath.Join(RepoDir(dataDir), "images", "capi", "packer", "proxmox", flavor.ID+".json")
	b, err := os.ReadFile(path)
	if err != nil {
		return isoSpec{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var spec isoSpec
	if err := json.Unmarshal(b, &spec); err != nil {
		return isoSpec{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return spec, nil
}

// isoFilename extracts the trailing path segment of an ISO URL, e.g.
// "https://releases.ubuntu.com/.../ubuntu-22.04.5-live-server-amd64.iso" ->
// "ubuntu-22.04.5-live-server-amd64.iso".
func isoFilename(isoURL string) string {
	if idx := strings.LastIndex(isoURL, "/"); idx >= 0 {
		return isoURL[idx+1:]
	}
	return isoURL
}

// proxmoxChecksumAlgo maps a Packer iso_checksum_type to the value Proxmox's
// download-url API expects. Packer's "file" type (checksum value is itself a
// URL to a checksum file, e.g. flatcar) has no Proxmox equivalent — callers
// should skip checksum verification in that case.
func proxmoxChecksumAlgo(packerType string) (algo string, ok bool) {
	switch packerType {
	case "sha256", "sha512", "sha1", "md5":
		return packerType, true
	default:
		return "", false
	}
}

// EnsureISOStagedStep pre-stages a flavor's installer ISO directly on
// Proxmox (rather than letting Packer download it into the container and
// then upload it to Proxmox over the REST API) and returns the ISO_FILE
// value ("<storage>:iso/<filename>") the build/validate steps should export,
// via isoFileOut. Uploading multi-GB ISOs through Proxmox's REST API is what
// causes "write: broken pipe" mid-build — Proxmox fetching the ISO itself
// server-side sidesteps that path entirely. Flavors whose upstream Packer
// config doesn't support ISO_FILE (flatcar, rockylinux) are left untouched.
func EnsureISOStagedStep(dataDir string, flavor OSFlavor, env ConnEnv, isoFileOut *string) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		spec, err := readISOSpec(dataDir, flavor)
		if err != nil {
			return err
		}
		if !strings.Contains(spec.IsoFile, "ISO_FILE") {
			c.Logf("%s doesn't support pre-staged ISOs upstream — Packer will download+upload as usual", flavor.Label)
			return nil
		}

		filename := isoFilename(spec.URL)
		client := env.proxmoxClient()

		has, err := client.HasISO(c, env.Node, env.ISOPool, filename)
		if err != nil {
			return fmt.Errorf("checking for existing ISO on Proxmox: %w", err)
		}
		if has {
			c.Logf("ISO already present on Proxmox: %s:iso/%s — skipping download and upload entirely", env.ISOPool, filename)
			if err := writeISOURLOverride(dataDir); err != nil {
				return err
			}
			*isoFileOut = env.ISOPool + ":iso/" + filename
			return nil
		}

		c.Logf("ISO not found on Proxmox storage %q — asking Proxmox to download it server-side (avoids the upload step that times out on large files)", env.ISOPool)
		c.Logf("Source: %s", spec.URL)

		algo, algoOK := proxmoxChecksumAlgo(spec.ChecksumType)
		checksum := spec.Checksum
		if !algoOK {
			c.Logf("Checksum type %q isn't verifiable by Proxmox's download-url API — downloading without verification", spec.ChecksumType)
			checksum = ""
		}

		upid, err := client.DownloadISOToStorage(c, env.Node, env.ISOPool, spec.URL, filename, checksum, algo)
		if err != nil {
			return fmt.Errorf("starting Proxmox-side ISO download: %w", err)
		}
		c.Logf("Download started (task %s), waiting for it to finish...", upid)

		if err := client.WaitTask(c, env.Node, upid, func(line string) { c.Logf("%s", line) }); err != nil {
			return fmt.Errorf("Proxmox ISO download failed: %w", err)
		}
		c.Logf("ISO downloaded to Proxmox: %s:iso/%s", env.ISOPool, filename)
		if err := writeISOURLOverride(dataDir); err != nil {
			return err
		}
		*isoFileOut = env.ISOPool + ":iso/" + filename
		return nil
	}
}

// isoURLOverrideFile is written into the image-builder checkout (bind-mounted
// into the container) whenever a flavor's ISO is pre-staged directly on
// Proxmox. It's passed to Packer via the Makefile's PACKER_VAR_FILES hook,
// which — unlike PACKER_FLAGS — is applied AFTER each flavor's own var-file
// (packer/proxmox/<flavor>.json), so it's the only way to actually clear
// that file's "iso_url". This is required: the proxmox-iso builder errors
// with "one of iso_file, iso_url... must be specified" when BOTH iso_file
// and iso_url are non-empty, rather than preferring iso_file — confirmed by
// direct testing against the container.
//
// It also bumps "boot_wait" past image-builder's 10s default. On slower
// Proxmox hosts, QEMU/BIOS POST + ISO read can eat into that budget, so
// Packer's boot_command (which presses "c" to interrupt GRUB and inject the
// autoinstall kernel param) lands after the installer already auto-booted
// into its normal interactive menu instead — the VM then sits at a language
// selection screen forever instead of running unattended. Confirmed live:
// the job log showed "Typing the boot command" followed by an indefinite
// stall at "Waiting for SSH to become available" while the Proxmox console
// was still on the installer's language menu.
const isoURLOverrideFile = ".pvekube-iso-override.json"

func writeISOURLOverride(dataDir string) error {
	path := filepath.Join(RepoDir(dataDir), "images", "capi", isoURLOverrideFile)
	return os.WriteFile(path, []byte(`{"iso_url": "", "boot_wait": "25s"}`+"\n"), 0o666)
}

// dockerRunArgs builds the full `docker run` invocation for a given make
// target. --net=host is required (Linux-only, enforced by the "os" prereq
// check) because Packer serves the autoinstall config over HTTP and the
// Proxmox build VM must be able to reach back to this container.
//
// The image's ENTRYPOINT is ["/usr/bin/make"] (verified via `docker
// inspect` — it is not a shell), so CMD here is just the make target name;
// no "bash -c" wrapper. Working directory is set with -w rather than `cd`
// for the same reason — there's no shell to run a `cd` in.
func dockerRunArgs(dataDir string, env ConnEnv, extraEnv []string, makeTarget, containerName string) []string {
	repoCapiDir := filepath.Join(RepoDir(dataDir), "images", "capi")
	isoCache := filepath.Join(dataDir, "iso-cache")
	os.MkdirAll(isoCache, 0o777)
	os.MkdirAll(repoCapiDir, 0o777)

	// Ensure all directories and subdirectories are world-writable so container can write
	chmodRecursive(isoCache, 0o777)
	chmodRecursive(repoCapiDir, 0o777)

	args := []string{"run", "--rm", "--net=host",
		"-w", "/home/imagebuilder/images/capi",
		"-v", repoCapiDir + ":/home/imagebuilder/images/capi",
		"-v", isoCache + ":/home/imagebuilder/images/capi/downloaded_iso_path",
	}
	// Naming the container is what makes a timed-out build recoverable:
	// killing the `docker run` client detaches from the container, it does
	// not stop it, so without a name there's no handle to reap Packer with.
	if containerName != "" {
		args = append(args, "--name", containerName)
	}
	args = append(args, env.dockerEnvArgs()...)
	for _, e := range extraEnv {
		args = append(args, "-e", e)
	}
	args = append(args, versions.ImageBuilderImage, makeTarget)
	return args
}

// chmodRecursive recursively sets permissions on all files/directories
func chmodRecursive(path string, mode os.FileMode) {
	filepath.Walk(path, func(p string, info os.FileInfo, err error) error {
		if err == nil {
			os.Chmod(p, mode)
		}
		return nil
	})
}

// ensurePackerJSON copies packer.json.tmpl -> packer.json on the HOST side
// of the bind mount (not inside the container, which has no shell to run a
// copy in — see dockerRunArgs). As of the pinned image-builder ref the repo
// ships the Packer template with a .tmpl suffix but the Makefile references
// it without one; Packer's legacy JSON template syntax ({{user `x`}} /
// {{env `X`}}) needs no separate rendering step, so a plain copy suffices.
func ensurePackerJSON(dataDir string) error {
	dir := filepath.Join(RepoDir(dataDir), "images", "capi", "packer", "proxmox")
	dst := filepath.Join(dir, "packer.json")
	if _, err := os.Stat(dst); err == nil {
		return nil
	}
	src := filepath.Join(dir, "packer.json.tmpl")
	b, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("reading %s: %w", src, err)
	}
	return os.WriteFile(dst, b, 0o644)
}

// ValidateSpec runs `make validate-proxmox-<flavor>` — a fast (seconds),
// side-effect-free syntax/config check that should always be run before
// committing to a 25-35 minute build.
func ValidateSpec(dataDir string, flavor OSFlavor, env ConnEnv, kv KubernetesVersion) *jobs.Spec {
	var isoFile string
	return jobs.NewSpec("template.validate", "Validate "+flavor.Label+" template config").
		Step("Ensure image-builder checkout", EnsureRepoStep(dataDir)).
		Step("Render packer.json", func(c *jobs.Ctx) error { return ensurePackerJSON(dataDir) }).
		Step("Stage installer ISO on Proxmox", EnsureISOStagedStep(dataDir, flavor, env, &isoFile)).
		Step("packer validate", func(c *jobs.Ctx) error {
			extraEnv := append(packerFlagsEnv(env, 0, kv), isoFileEnv(isoFile)...)
			args := dockerRunArgs(dataDir, env, extraEnv, "validate-proxmox-"+flavor.ID, "")
			return runner.Run(c, c, "", nil, "docker", args...)
		})
}

// BuildTimeout bounds a single template build. Generous — a slow host can
// legitimately spend 35+ minutes here — but finite, because the failure mode
// it exists for produces no output at all and would otherwise wait forever.
const BuildTimeout = 90 * time.Minute

// BuildSpec runs the real build. vmid is pre-allocated by the caller via
// the Proxmox client (rather than left to Packer's default of "next free
// ID at boot time") so PVEKube knows deterministically which VM/template
// the result is, instead of parsing it back out of Packer's log output.
func BuildSpec(dataDir string, flavor OSFlavor, kv KubernetesVersion, vmid int, env ConnEnv) *jobs.Spec {
	var isoFile string
	container := fmt.Sprintf("pvekube-build-%d", vmid)
	title := "Build " + flavor.Label + " template (VMID " + fmt.Sprint(vmid) + ")"
	if kv.Requested() {
		title += " for Kubernetes " + kv.Semver
	}
	return jobs.NewSpec("template.build", title).
		Step("Ensure image-builder checkout", EnsureRepoStep(dataDir)).
		Step("Render packer.json", func(c *jobs.Ctx) error { return ensurePackerJSON(dataDir) }).
		Step("Stage installer ISO on Proxmox", EnsureISOStagedStep(dataDir, flavor, env, &isoFile)).
		Step("packer validate (pre-flight)", func(c *jobs.Ctx) error {
			extraEnv := append(packerFlagsEnv(env, 0, kv), isoFileEnv(isoFile)...)
			args := dockerRunArgs(dataDir, env, extraEnv, "validate-proxmox-"+flavor.ID, "")
			return runner.Run(c, c, "", nil, "docker", args...)
		}).
		Step("packer build (20-35 minutes)", func(c *jobs.Ctx) error {
			c.Logf("Building on node=%s storage=%s (format=%s) bridge=%s iso_pool=%s vmid=%d", env.Node, env.StoragePool, diskFormatOrDefault(env.DiskFormat), env.Bridge, env.ISOPool, vmid)
			if kv.Requested() {
				c.Logf("Kubernetes %s (deb %s, rpm %s, series %s)", kv.Semver, kv.DebVersion, kv.RPMVersion, kv.Series)
			} else {
				c.Logf("Kubernetes version: image-builder's pinned default (no version was requested)")
			}
			extraEnv := append(packerFlagsEnv(env, vmid, kv), isoFileEnv(isoFile)...)
			args := dockerRunArgs(dataDir, env, extraEnv, "build-proxmox-"+flavor.ID, container)

			// Bounded so a wedged Packer fails instead of hanging forever.
			// This is not hypothetical: image-builder's own packer.json
			// reboots the guest and then allows a fixed 10s before running
			// Ansible, and on a loaded host the VM isn't back in time. Packer
			// then reports "ssh: handshake failed: EOF" and the Ansible ssh
			// client blocks indefinitely — its ConnectTimeout only covers the
			// TCP connect, which succeeds instantly against Packer's local
			// proxy adapter, so nothing bounds the dead handshake. Observed
			// live: a build sat wedged with flat CPU until killed by hand,
			// and because templateBuildInProgress (handlers_templates.go)
			// treats a running build as a global lock, it blocked every
			// subsequent build on every connection too.
			ctx, cancel := context.WithTimeout(c, BuildTimeout)
			defer cancel()
			err := runner.Run(ctx, c, "", nil, "docker", args...)

			if ctx.Err() == context.DeadlineExceeded {
				// By this point runner.Run has already tried the graceful
				// path: it sends SIGTERM (not SIGKILL) on ctx cancellation,
				// docker's CLI forwards that into the container, and Packer
				// catches it to stop+delete the build VM itself before
				// exiting — confirmed live, this is the common case and it
				// finishes in a few seconds. `docker rm -f` here is only the
				// fallback for when that graceful shutdown didn't finish
				// within runner.Run's WaitDelay: killing the LOCAL `docker
				// run` client detaches rather than stopping the container,
				// which would otherwise keep running (and keep holding the
				// build VM) after the job has already failed.
				c.Logf("Build exceeded %s — ensuring container %s is stopped", BuildTimeout, container)
				rm := exec.Command("docker", "rm", "-f", container)
				rm.CombinedOutput() // best-effort; a "no such container" failure here just means Packer's own graceful cleanup (above) already removed it

				// The container's fate says nothing definitive about the
				// Proxmox VM it created — Packer may have gotten far enough
				// to delete the container but not the VM, or vice versa. Only
				// asserting cleanup status after checking Proxmox directly is
				// what fixed a real false report: this exact path once
				// logged "VM was left behind and needs deleting by hand" for
				// a VM Packer's own graceful shutdown had already deleted.
				return reportBuildTimeout(c, env, vmid)
			}
			return err
		})
}

// reportBuildTimeout checks Proxmox directly for whether the build VM
// actually still exists before deciding what to tell the operator, rather
// than assuming either outcome. Three distinct cases, three distinct
// messages: confirmed gone (nothing to do), confirmed still there (name the
// exact VMID to delete), or unknown (the check itself failed — say so rather
// than guessing, since a network hiccup here must never be reported as
// either a successful or a failed cleanup).
func reportBuildTimeout(c *jobs.Ctx, env ConnEnv, vmid int) error {
	client := proxmox.New(proxmox.Config{URL: env.URL, TokenID: env.TokenID, Secret: env.Secret, InsecureSkipVerify: env.InsecureTLS})
	checkCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	exists, err := client.VMExists(checkCtx, env.Node, vmid)
	switch {
	case err != nil:
		c.Logf("Could not confirm whether Proxmox VM %d still exists: %v — check node %s by hand.", vmid, err, env.Node)
	case exists:
		c.Logf("Proxmox VM %d is still present on node %s and needs deleting by hand before retrying.", vmid, env.Node)
	default:
		c.Logf("Confirmed: Proxmox VM %d no longer exists — Packer's own cleanup already removed it, nothing further to do.", vmid)
	}
	return fmt.Errorf("template build exceeded %s and was aborted", BuildTimeout)
}

// diskFormatOrDefault falls back to image-builder's own Packer template
// default ("qcow2") when the caller didn't resolve a storage pool's format.
func diskFormatOrDefault(f string) string {
	if f == "" {
		return "qcow2"
	}
	return f
}

// packerFlagsEnv builds the single "PACKER_FLAGS=..." docker -e argument.
// vmid of 0 omits --var vmid (used for validate, which never allocates a
// real VM). disk_format is always passed explicitly — LVM-thin/ZFS storage
// pools reject image-builder's unconditional "qcow2" default outright
// ("unsupported format 'qcow2'"), so this can't be left unset even though
// nothing else in the var-file chain happens to override it.
func packerFlagsEnv(env ConnEnv, vmid int, kv KubernetesVersion) []string {
	flags := "--var disk_format=" + diskFormatOrDefault(env.DiskFormat)
	if vmid > 0 {
		flags += fmt.Sprintf(" --var vmid=%d", vmid)
	}
	// extra_debs is image-builder's own supported hook (roles/setup/tasks/debian.yml,
	// consumed early enough that sysprep's package-pinning step still catches it) for
	// packages a Kubernetes node needs that aren't in its base Ansible role set.
	// nfs-common (mount.nfs, for CSI drivers backed by NFS storage classes) is the one
	// gap found by inspecting a live build's "Pin all installed packages" task, which
	// enumerates every package already on the base OS image — open-iscsi is already
	// present there (part of Ubuntu Server's stock package set), so it's deliberately
	// not duplicated here. Debian-only; harmless no-op on RPM/Flatcar flavors.
	flags += ` --var extra_debs="nfs-common"`
	// Empty unless an explicit version was requested, so a default build
	// still runs on image-builder's own pinned versions exactly as before.
	flags += kv.packerVars()
	return []string{"PACKER_FLAGS=" + flags}
}

// isoFileEnv returns the ISO_FILE docker -e argument when a flavor's ISO was
// pre-staged directly on Proxmox, or nil if it wasn't (either the flavor
// doesn't support it, or staging is left to Packer's own download+upload).
func isoFileEnv(isoFile string) []string {
	if isoFile == "" {
		return nil
	}
	// PACKER_VAR_FILES (a Makefile hook, auto-imported from the environment
	// like any other Make variable) is applied AFTER the flavor's own
	// var-file, so it's what actually clears "iso_url" — see
	// writeISOURLOverride for why that's required alongside ISO_FILE.
	return []string{"ISO_FILE=" + isoFile, "PACKER_VAR_FILES=" + isoURLOverrideFile}
}
