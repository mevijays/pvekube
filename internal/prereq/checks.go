// Package prereq defines the prerequisite checklist: what must be true on
// the app host before templates can be built or clusters launched, how to
// check each one cheaply and safely, and — where it's safe to automate —
// how to fix it.
package prereq

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"pvekube/internal/bootstrap"
	"pvekube/internal/versions"
)

type Status string

const (
	StatusOK      Status = "ok"
	StatusFail    Status = "fail"
	StatusWarn    Status = "warn"
	StatusUnknown Status = "unknown"
)

type Result struct {
	ID      string
	Name    string
	Status  Status
	Detail  string
	Fixable bool
}

// Check is one prerequisite. Run must be fast (<2s) and side-effect free —
// it's called on every page load / re-check. Fix (if FixJobKind is set) is
// a job kind registered separately in the jobs system; the prereq package
// only declares that a fix exists, it doesn't run it (avoids an import
// cycle with jobs, and keeps remediation logic colocated with each tool's
// own package where it already lives, e.g. bootstrap.InstallKind).
type Check struct {
	ID      string
	Name    string
	Fixable bool
	Run     func(ctx context.Context) Result
}

// Registry returns the ordered list of checks that make up the "local host"
// prerequisites screen. Proxmox connectivity checks are added separately
// once a connection profile exists (see internal/proxmox).
func Registry(binDir, dataDir string) []Check {
	return []Check{
		checkOS(),
		checkInternet(),
		checkDiskSpace(binDir),
		checkDocker(),
		checkDockerRunning(),
		checkBinary("kind", binDir, versions.Kind),
		checkBinary("clusterctl", binDir, versions.Clusterctl),
		checkBinary("kubectl", binDir, versions.Kubectl),
		checkImageBuilderImage(),
		checkKindCluster(binDir, dataDir),
		checkCAPIProviders(binDir, dataDir),
	}
}

func result(id, name string, status Status, detail string, fixable bool) Result {
	return Result{ID: id, Name: name, Status: status, Detail: detail, Fixable: fixable}
}

func checkOS() Check {
	return Check{
		ID: "os", Name: "Operating system", Fixable: false,
		Run: func(ctx context.Context) Result {
			if runtime.GOOS != "linux" {
				return result("os", "Operating system", StatusFail,
					fmt.Sprintf("Running on %s. Template building requires Linux (the Proxmox build VM must reach back to this host over HTTP, which needs Docker's --net=host — Linux only). Cluster launching works on any OS once a template already exists.", runtime.GOOS),
					false)
			}
			return result("os", "Operating system", StatusOK,
				fmt.Sprintf("Linux/%s — supported.", runtime.GOARCH), false)
		},
	}
}

func checkInternet() Check {
	return Check{
		ID: "internet", Name: "Internet access", Fixable: false,
		Run: func(ctx context.Context) Result {
			hosts := []string{"registry.k8s.io:443", "github.com:443"}
			for _, h := range hosts {
				d := net.Dialer{Timeout: 4 * time.Second}
				conn, err := d.DialContext(ctx, "tcp", h)
				if err != nil {
					return result("internet", "Internet access", StatusFail,
						fmt.Sprintf("Cannot reach %s: %v. This host needs outbound HTTPS to download tool binaries and container images.", h, err), false)
				}
				conn.Close()
			}
			return result("internet", "Internet access", StatusOK, "Reached registry.k8s.io and github.com.", false)
		},
	}
}

func checkDiskSpace(dir string) Check {
	return Check{
		ID: "disk", Name: "Free disk space", Fixable: false,
		Run: func(ctx context.Context) Result {
			free, err := freeBytes(dir)
			if err != nil {
				return result("disk", "Free disk space", StatusWarn, fmt.Sprintf("Could not determine free space: %v", err), false)
			}
			const minGB = 40
			freeGB := free / (1024 * 1024 * 1024)
			if freeGB < minGB {
				return result("disk", "Free disk space", StatusFail,
					fmt.Sprintf("%d GB free, want at least %d GB (VM template builds and container images need room).", freeGB, minGB), false)
			}
			return result("disk", "Free disk space", StatusOK, fmt.Sprintf("%d GB free.", freeGB), false)
		},
	}
}

func checkDocker() Check {
	return Check{
		ID: "docker_installed", Name: "Docker installed", Fixable: true,
		Run: func(ctx context.Context) Result {
			path, err := exec.LookPath("docker")
			if err != nil {
				return result("docker_installed", "Docker installed", StatusFail,
					"Docker is not installed. PVEKube uses it to run the image-builder toolchain and the local cluster bootstrap.", true)
			}
			return result("docker_installed", "Docker installed", StatusOK, "Found at "+path+".", false)
		},
	}
}

func checkDockerRunning() Check {
	return Check{
		ID: "docker_running", Name: "Docker daemon running", Fixable: true,
		Run: func(ctx context.Context) Result {
			cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			cmd := exec.CommandContext(cctx, "docker", "info")
			out, err := cmd.CombinedOutput()
			if err != nil {
				return result("docker_running", "Docker daemon running", StatusFail,
					fmt.Sprintf("`docker info` failed: %s", firstLine(string(out), err)), true)
			}
			return result("docker_running", "Docker daemon running", StatusOK, "Docker daemon is responding.", false)
		},
	}
}

func checkBinary(name, binDir, wantVersion string) Check {
	id := "bin_" + name
	return Check{
		ID: id, Name: name + " CLI (" + wantVersion + ")", Fixable: true,
		Run: func(ctx context.Context) Result {
			p := binDir + "/" + name
			if _, err := os.Stat(p); err != nil {
				return result(id, name+" CLI", StatusFail,
					fmt.Sprintf("Not downloaded yet. Will fetch %s %s and verify its checksum.", name, wantVersion), true)
			}
			return result(id, name+" CLI", StatusOK, "Present at "+p+".", false)
		},
	}
}

func checkImageBuilderImage() Check {
	return Check{
		ID: "img_imagebuilder", Name: "image-builder container image", Fixable: true,
		Run: func(ctx context.Context) Result {
			cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
			defer cancel()
			cmd := exec.CommandContext(cctx, "docker", "image", "inspect", versions.ImageBuilderImage)
			if err := cmd.Run(); err != nil {
				return result("img_imagebuilder", "image-builder image", StatusFail,
					"Not pulled yet: "+versions.ImageBuilderImage, true)
			}
			return result("img_imagebuilder", "image-builder image", StatusOK, "Pulled: "+versions.ImageBuilderImage, false)
		},
	}
}

func checkKindCluster(binDir, dataDir string) Check {
	return Check{
		ID: "kind_cluster", Name: "Local management cluster (KIND)", Fixable: true,
		Run: func(ctx context.Context) Result {
			kindBin := filepath.Join(binDir, "kind")
			if _, err := os.Stat(kindBin); err != nil {
				return result("kind_cluster", "Local management cluster (KIND)", StatusFail,
					"Waiting on the kind CLI to be downloaded first.", false)
			}
			running, err := bootstrap.IsClusterRunning(kindBin)
			if err != nil {
				return result("kind_cluster", "Local management cluster (KIND)", StatusFail,
					"Could not check: "+err.Error(), true)
			}
			if !running {
				return result("kind_cluster", "Local management cluster (KIND)", StatusFail,
					fmt.Sprintf("No %q cluster found. This is where Cluster API's controllers run — it never hosts your workloads.", bootstrap.ClusterName), true)
			}
			return result("kind_cluster", "Local management cluster (KIND)", StatusOK,
				fmt.Sprintf("Cluster %q is running.", bootstrap.ClusterName), false)
		},
	}
}

func checkCAPIProviders(binDir, dataDir string) Check {
	return Check{
		ID: "capi_providers", Name: "Cluster API providers installed", Fixable: true,
		Run: func(ctx context.Context) Result {
			kubectlBin := filepath.Join(binDir, "kubectl")
			if _, err := os.Stat(kubectlBin); err != nil {
				return result("capi_providers", "Cluster API providers installed", StatusFail,
					"Waiting on kubectl and the management cluster first.", false)
			}
			healthy, detail, err := bootstrap.ProvidersHealthy(kubectlBin, dataDir)
			if err != nil {
				return result("capi_providers", "Cluster API providers installed", StatusFail,
					"Not installed yet, or cluster unreachable: "+firstLine(err.Error(), err), true)
			}
			if !healthy {
				return result("capi_providers", "Cluster API providers installed", StatusFail,
					"Installed but not all healthy: "+detail, true)
			}
			return result("capi_providers", "Cluster API providers installed", StatusOK,
				fmt.Sprintf("core %s + proxmox %s + ipam %s all Available.", versions.Default.CAPICore, versions.Default.CAPMOX, versions.Default.IPAMInCluster), false)
		},
	}
}

func firstLine(s string, fallbackErr error) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	if s != "" {
		return s
	}
	return fallbackErr.Error()
}

// httpProbe is kept for reuse by future checks (e.g. reachability from
// Proxmox back to this host) that need a real GET, not just a TCP dial.
func httpProbe(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}
