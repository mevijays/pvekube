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
	"fmt"
	"os"
	"path/filepath"

	"pvekube/internal/jobs"
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
	Node        string
	ISOPool     string
	Bridge      string
	StoragePool string
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

// dockerRunArgs builds the full `docker run` invocation for a given make
// target. --net=host is required (Linux-only, enforced by the "os" prereq
// check) because Packer serves the autoinstall config over HTTP and the
// Proxmox build VM must be able to reach back to this container.
//
// The image's ENTRYPOINT is ["/usr/bin/make"] (verified via `docker
// inspect` — it is not a shell), so CMD here is just the make target name;
// no "bash -c" wrapper. Working directory is set with -w rather than `cd`
// for the same reason — there's no shell to run a `cd` in.
func dockerRunArgs(dataDir string, env ConnEnv, extraEnv []string, makeTarget string) []string {
	repoCapiDir := filepath.Join(RepoDir(dataDir), "images", "capi")
	isoCache := filepath.Join(dataDir, "iso-cache")
	os.MkdirAll(isoCache, 0o755)

	args := []string{"run", "--rm", "--net=host",
		"-w", "/home/imagebuilder/images/capi",
		"-v", repoCapiDir + ":/home/imagebuilder/images/capi",
		"-v", isoCache + ":/home/imagebuilder/images/capi/downloaded_iso_path",
	}
	args = append(args, env.dockerEnvArgs()...)
	for _, e := range extraEnv {
		args = append(args, "-e", e)
	}
	args = append(args, versions.ImageBuilderImage, makeTarget)
	return args
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
func ValidateSpec(dataDir string, flavor OSFlavor, env ConnEnv) *jobs.Spec {
	return jobs.NewSpec("template.validate", "Validate "+flavor.Label+" template config").
		Step("Ensure image-builder checkout", EnsureRepoStep(dataDir)).
		Step("Render packer.json", func(c *jobs.Ctx) error { return ensurePackerJSON(dataDir) }).
		Step("packer validate", func(c *jobs.Ctx) error {
			args := dockerRunArgs(dataDir, env, nil, "validate-proxmox-"+flavor.ID)
			return runner.Run(c, c, "", nil, "docker", args...)
		})
}

// BuildSpec runs the real build. vmid is pre-allocated by the caller via
// the Proxmox client (rather than left to Packer's default of "next free
// ID at boot time") so PVEKube knows deterministically which VM/template
// the result is, instead of parsing it back out of Packer's log output.
func BuildSpec(dataDir string, flavor OSFlavor, k8sVersion string, vmid int, env ConnEnv) *jobs.Spec {
	extraEnv := []string{fmt.Sprintf("PACKER_FLAGS=--var vmid=%d", vmid)}
	return jobs.NewSpec("template.build", "Build "+flavor.Label+" template (VMID "+fmt.Sprint(vmid)+")").
		Step("Ensure image-builder checkout", EnsureRepoStep(dataDir)).
		Step("Render packer.json", func(c *jobs.Ctx) error { return ensurePackerJSON(dataDir) }).
		Step("packer validate (pre-flight)", func(c *jobs.Ctx) error {
			args := dockerRunArgs(dataDir, env, nil, "validate-proxmox-"+flavor.ID)
			return runner.Run(c, c, "", nil, "docker", args...)
		}).
		Step("packer build (20-35 minutes)", func(c *jobs.Ctx) error {
			c.Logf("Building on node=%s storage=%s bridge=%s iso_pool=%s vmid=%d", env.Node, env.StoragePool, env.Bridge, env.ISOPool, vmid)
			args := dockerRunArgs(dataDir, env, extraEnv, "build-proxmox-"+flavor.ID)
			return runner.Run(c, c, "", nil, "docker", args...)
		})
}
