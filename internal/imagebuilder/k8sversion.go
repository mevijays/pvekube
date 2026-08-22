// Resolving an operator-supplied Kubernetes version into the variables
// image-builder's Packer/Ansible chain actually consumes.
//
// Pinning a version is not one variable but four, and only three of them
// can be computed. Ansible installs the Debian packages with an EXACT apt
// pin (roles/kubernetes/tasks/debian.yml: "kubeadm={{ kubernetes_deb_version }}"),
// so that value has to include upstream's packaging revision — and that
// revision is NOT a constant. Read straight off pkgs.k8s.io, the v1.36
// series ships 1.36.0-1.1, 1.36.1-1.1, 1.36.2-2.1, 1.36.3-1.1: the ".2"
// patch was repackaged and carries -2.1. Deriving "-1.1" by convention
// would produce an apt pin that doesn't exist and fail the build ~25
// minutes in, at package install, with nothing pointing back at the real
// cause. So the deb version is discovered from the repository index
// instead of assumed.
//
// The RPM side needs no such lookup: those tasks use "kubeadm-{{
// kubernetes_rpm_version }}", which yum resolves to whatever release
// exists, so the bare patch version is enough.
package imagebuilder

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// KubernetesVersion is the resolved set of image-builder variables for one
// requested Kubernetes version. A zero value means "not requested" — the
// build then runs on image-builder's own pinned defaults, exactly as it did
// before version selection existed.
type KubernetesVersion struct {
	Semver     string // v1.36.2  -> kubernetes_semver
	Series     string // v1.36    -> kubernetes_series (builds the pkgs.k8s.io repo URL)
	DebVersion string // 1.36.2-2.1 -> kubernetes_deb_version (exact apt pin)
	RPMVersion string // 1.36.2   -> kubernetes_rpm_version
}

// Requested reports whether an explicit version was resolved.
func (k KubernetesVersion) Requested() bool { return k.Semver != "" }

// packerVars renders the "--var name=value" fragments Packer needs. Empty
// when no version was requested, which keeps the default build path
// byte-identical to before this existed.
func (k KubernetesVersion) packerVars() string {
	if !k.Requested() {
		return ""
	}
	return fmt.Sprintf(" --var kubernetes_semver=%s --var kubernetes_series=%s --var kubernetes_deb_version=%s --var kubernetes_rpm_version=%s",
		k.Semver, k.Series, k.DebVersion, k.RPMVersion)
}

// debPackagesURL is the flat-repo index listing every package version in a
// Kubernetes minor series. A var, not a func, so tests can point resolution
// at a local fixture instead of reaching out to pkgs.k8s.io.
var debPackagesURL = func(series string) string {
	return "https://pkgs.k8s.io/core:/stable:/" + series + "/deb/Packages"
}

// ResolveKubernetesVersion turns "1.36.2" or "v1.36.2" into the full
// variable set, verifying against upstream that the version actually ships
// as a package. Returning an error here means the operator finds out in
// seconds, at the form, instead of ~25 minutes into a build.
//
// An empty request is not an error: it yields the zero value, meaning
// "leave image-builder's own pinned defaults alone".
func ResolveKubernetesVersion(ctx context.Context, requested string) (KubernetesVersion, error) {
	bare := strings.TrimSpace(requested)
	if bare == "" {
		return KubernetesVersion{}, nil
	}
	bare = strings.TrimPrefix(bare, "v")

	parts := strings.Split(bare, ".")
	if len(parts) != 3 {
		return KubernetesVersion{}, fmt.Errorf("Kubernetes version %q must look like 1.36.2 or v1.36.2 (major.minor.patch)", requested)
	}
	for _, p := range parts {
		if p == "" {
			return KubernetesVersion{}, fmt.Errorf("Kubernetes version %q must look like 1.36.2 or v1.36.2 (major.minor.patch)", requested)
		}
		if _, err := strconv.Atoi(p); err != nil {
			return KubernetesVersion{}, fmt.Errorf("Kubernetes version %q must look like 1.36.2 or v1.36.2 (major.minor.patch)", requested)
		}
	}
	series := "v" + parts[0] + "." + parts[1]

	available, err := kubeadmDebVersions(ctx, series)
	if err != nil {
		return KubernetesVersion{}, err
	}
	if len(available) == 0 {
		return KubernetesVersion{}, fmt.Errorf("no Kubernetes packages found for series %s — check that %s is a real Kubernetes minor version", series, series)
	}

	// Match on the "<version>-" prefix so the packaging revision, whatever
	// it turns out to be, comes from upstream rather than from a guess.
	for _, v := range available {
		if strings.HasPrefix(v, bare+"-") {
			return KubernetesVersion{
				Semver:     "v" + bare,
				Series:     series,
				DebVersion: v,
				RPMVersion: bare,
			}, nil
		}
	}

	patches := make([]string, 0, len(available))
	for _, v := range available {
		if i := strings.Index(v, "-"); i > 0 {
			patches = append(patches, v[:i])
		}
	}
	return KubernetesVersion{}, fmt.Errorf("Kubernetes %s is not published in the %s package repository — available: %s",
		bare, series, strings.Join(dedupeSorted(patches), ", "))
}

// kubeadmDebVersions reads every kubeadm version in a series' flat Debian
// repository index. kubeadm (not kubelet/kubectl) is the reference package
// because it's the one whose exact pin decides the cluster's version.
func kubeadmDebVersions(ctx context.Context, series string) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, debPackagesURL(series), nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("checking which Kubernetes versions exist for %s: %w", series, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no Kubernetes package repository exists for series %s — check the version you entered", series)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("checking which Kubernetes versions exist for %s: HTTP %d from %s", series, resp.StatusCode, debPackagesURL(series))
	}

	// A Debian Packages index is stanzas of "Key: value" separated by blank
	// lines. Only kubeadm's Version lines matter; the same version repeats
	// once per architecture, which dedupeSorted collapses.
	var out []string
	var isKubeadm bool
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.TrimSpace(line) == "":
			isKubeadm = false
		case line == "Package: kubeadm":
			isKubeadm = true
		case isKubeadm && strings.HasPrefix(line, "Version: "):
			out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "Version: ")))
			isKubeadm = false
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("reading the %s package index: %w", series, err)
	}
	return dedupeSorted(out), nil
}

func dedupeSorted(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
