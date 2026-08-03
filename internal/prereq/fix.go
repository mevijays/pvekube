// Remediation jobs for each fixable check. Kept in this package (rather than
// under internal/jobs) because the download URLs/checksums are inherently
// tied to the specific tool version each check validates against.
package prereq

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

	"pvekube/internal/bootstrap"
	"pvekube/internal/jobs"
	"pvekube/internal/runner"
	"pvekube/internal/versions"
)

// BuildFixSpec returns the job Spec that remediates the given check ID, or
// nil if that check has no automated fix.
func BuildFixSpec(checkID, binDir, dataDir string) *jobs.Spec {
	switch checkID {
	case "docker_installed":
		return dockerInstallSpec()
	case "docker_running":
		return dockerStartSpec()
	case "bin_kind":
		return binaryDownloadSpec("kind", binDir, versions.Kind, kindDownloadURL)
	case "bin_clusterctl":
		return binaryDownloadSpec("clusterctl", binDir, versions.Clusterctl, clusterctlDownloadURL)
	case "bin_kubectl":
		return binaryDownloadSpec("kubectl", binDir, versions.Kubectl, kubectlDownloadURL)
	case "img_imagebuilder":
		return pullImageSpec()
	case "kind_cluster":
		return bootstrap.CreateClusterSpec(filepath.Join(binDir, "kind"), dataDir)
	case "capi_providers":
		return bootstrap.InitSpec(filepath.Join(binDir, "clusterctl"), filepath.Join(binDir, "kind"), dataDir)
	default:
		return nil
	}
}

func dockerInstallSpec() *jobs.Spec {
	return jobs.NewSpec("prereq.install_docker", "Install Docker").
		Step("Download and run get.docker.com", func(c *jobs.Ctx) error {
			c.Logf("Installing Docker via the official convenience script (get.docker.com).")
			c.Logf("This requires sudo; you may be prompted on the host's console if passwordless sudo isn't configured.")
			return runner.Run(c, c, "", nil, "sh", "-c", "curl -fsSL https://get.docker.com | sh")
		}).
		Step("Add current user to docker group", func(c *jobs.Ctx) error {
			user := os.Getenv("USER")
			if user == "" {
				c.Logf("Could not determine current user; skipping group add. You may need to run docker with sudo.")
				return nil
			}
			return runner.Run(c, c, "", nil, "sudo", "usermod", "-aG", "docker", user)
		})
}

func dockerStartSpec() *jobs.Spec {
	return jobs.NewSpec("prereq.start_docker", "Start Docker daemon").
		Step("Start docker service", func(c *jobs.Ctx) error {
			if err := runner.Run(c, c, "", nil, "sudo", "systemctl", "start", "docker"); err != nil {
				c.Logf("systemctl start failed (%v); this host may not use systemd. Start Docker Desktop or the docker daemon manually.", err)
				return err
			}
			return nil
		})
}

func pullImageSpec() *jobs.Spec {
	return jobs.NewSpec("prereq.pull_imagebuilder", "Pull image-builder image").
		Step("docker pull "+versions.ImageBuilderImage, func(c *jobs.Ctx) error {
			return runner.Run(c, c, "", nil, "docker", "pull", versions.ImageBuilderImage)
		})
}

// --- binary download + checksum verify ---
//
// Only kubectl has a stable, well-documented per-asset checksum contract
// (a "<url>.sha256" sidecar containing the bare hex digest). kind and
// clusterctl don't publish an equivalent per-binary sidecar as of the
// versions pinned here, so for those we say so explicitly rather than
// silently skipping verification and implying it happened.
func binaryDownloadSpec(name, binDir, version string, urlFn func(os, arch, version string) string) *jobs.Spec {
	return jobs.NewSpec("prereq.download_"+name, "Download "+name+" "+version).
		Step("Download "+name, func(c *jobs.Ctx) error {
			goos, arch := runtime.GOOS, runtime.GOARCH
			url := urlFn(goos, arch, version)
			c.Logf("Downloading %s from %s", name, url)
			dest := filepath.Join(binDir, name)
			digest, err := downloadFile(c, url, dest)
			if err != nil {
				return err
			}
			if name == "kubectl" {
				if err := verifyKubectlChecksum(c, url, digest); err != nil {
					os.Remove(dest)
					return err
				}
				c.Logf("Checksum verified against %s.sha256", url)
			} else {
				c.Logf("Note: %s does not publish a per-asset checksum sidecar; integrity relies on HTTPS + GitHub's release signing, not a local hash check.", name)
			}
			if err := os.Chmod(dest, 0o755); err != nil {
				return err
			}
			c.Logf("Saved to %s (sha256 %s)", dest, digest)
			return nil
		})
}

// downloadFile saves url to dest and returns the hex sha256 digest of what
// was actually written, so callers can verify it against a known-good value.
func downloadFile(ctx context.Context, url, dest string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("downloading %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("downloading %s: HTTP %d", url, resp.StatusCode)
	}

	tmp := dest + ".download"
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", err
	}
	f.Close()
	digest := hex.EncodeToString(h.Sum(nil))
	if err := os.Rename(tmp, dest); err != nil {
		return "", err
	}
	return digest, nil
}

func verifyKubectlChecksum(ctx context.Context, binURL, gotDigest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, binURL+".sha256", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("fetching checksum sidecar: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetching checksum sidecar: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	want := firstToken(string(body))
	if want == "" {
		return fmt.Errorf("checksum sidecar was empty")
	}
	if want != gotDigest {
		return fmt.Errorf("checksum mismatch: sidecar says %s, downloaded file hashes to %s — refusing to trust this binary", want, gotDigest)
	}
	return nil
}

func firstToken(s string) string {
	for i, c := range s {
		if c == ' ' || c == '\n' || c == '\t' {
			return s[:i]
		}
	}
	return s
}

func kindDownloadURL(goos, arch, version string) string {
	return fmt.Sprintf("https://kind.sigs.k8s.io/dl/%s/kind-%s-%s", version, goos, arch)
}

func clusterctlDownloadURL(goos, arch, version string) string {
	return fmt.Sprintf("https://github.com/kubernetes-sigs/cluster-api/releases/download/%s/clusterctl-%s-%s", version, goos, arch)
}

func kubectlDownloadURL(goos, arch, version string) string {
	return fmt.Sprintf("https://dl.k8s.io/release/%s/bin/%s/%s/kubectl", version, goos, arch)
}
