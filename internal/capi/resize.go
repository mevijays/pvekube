// Resizing the machines of an existing workload cluster.
//
// Cluster API does not resize a running VM. A ProxmoxMachineTemplate is
// immutable, so a new size means a NEW template object carrying the new
// numSockets/numCores/memoryMiB, and pointing the KubeadmControlPlane and/or
// MachineDeployment at it makes Cluster API replace every machine of that role
// with a freshly cloned VM — exactly the mechanism a Kubernetes upgrade uses
// (see upgrade.go), just without a version or template-VM change.
//
// The new machine template is CLONED from the live one for the same reason an
// upgrade clones: everything PVEKube or the operator put on it (boot volume,
// checks.skipCloudInitStatus for Flatcar, the template VM and node, any CAPMOX
// field this code has never heard of) survives untouched. Only the sizes the
// operator asked to change are written.
//
// Nothing is resized in place, so anything stored on a node-local disk is
// destroyed together with the VM it lives on. The UI makes the operator
// acknowledge that; this package only does what it is told.
package capi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"pvekube/internal/bootstrap"
	"pvekube/internal/jobs"
	"pvekube/internal/runner"
)

// Sanity bounds. They exist to catch typos (a RAM value entered in MiB where
// GiB was meant, or the other way round), not to model Proxmox capacity.
const (
	minResizeMemoryMiB = 2048    // kubeadm's preflight wants ~1.7 GiB on a control plane
	maxResizeMemoryMiB = 1 << 20 // 1 TiB
	maxResizeCores     = 128
	maxResizeSockets   = 8
)

// ResizeRequest is the size one role should get. A zero field means "leave
// that value as it is", so asking for only memoryMiB changes only memory.
type ResizeRequest struct {
	NumSockets int
	NumCores   int
	MemoryMiB  int
}

// IsZero reports whether nothing was requested for the role.
func (r ResizeRequest) IsZero() bool { return r == ResizeRequest{} }

// ResizeInput is a whole resize: one optional request per role.
type ResizeInput struct {
	ClusterName  string
	ControlPlane ResizeRequest
	Workers      ResizeRequest
}

// Describe is a short human summary, used in the job title and log.
func (r ResizeRequest) Describe() string {
	var parts []string
	if r.MemoryMiB > 0 {
		parts = append(parts, fmt.Sprintf("%s RAM", FormatMiB(r.MemoryMiB)))
	}
	if r.NumCores > 0 {
		parts = append(parts, fmt.Sprintf("%d cores", r.NumCores))
	}
	if r.NumSockets > 0 {
		parts = append(parts, fmt.Sprintf("%d sockets", r.NumSockets))
	}
	return strings.Join(parts, ", ")
}

// FormatMiB renders a MiB count the way operators think about RAM:
// "25 GiB" when it is a whole number of GiB, otherwise "16392 MiB".
func FormatMiB(mib int) string {
	if mib > 0 && mib%1024 == 0 {
		return fmt.Sprintf("%d GiB", mib/1024)
	}
	return fmt.Sprintf("%d MiB", mib)
}

// ValidateResize rejects requests that are empty or out of range.
func ValidateResize(in ResizeInput) error {
	if in.ControlPlane.IsZero() && in.Workers.IsZero() {
		return fmt.Errorf("enter a new RAM, core or socket count for at least one of the control plane or the workers")
	}
	for role, r := range map[string]ResizeRequest{"control plane": in.ControlPlane, "workers": in.Workers} {
		if r.MemoryMiB != 0 && (r.MemoryMiB < minResizeMemoryMiB || r.MemoryMiB > maxResizeMemoryMiB) {
			return fmt.Errorf("%s: RAM must be between %s and %s", role, FormatMiB(minResizeMemoryMiB), FormatMiB(maxResizeMemoryMiB))
		}
		if r.NumCores < 0 || r.NumCores > maxResizeCores {
			return fmt.Errorf("%s: cores must be between 1 and %d", role, maxResizeCores)
		}
		if r.NumSockets < 0 || r.NumSockets > maxResizeSockets {
			return fmt.Errorf("%s: sockets must be between 1 and %d", role, maxResizeSockets)
		}
		if r.MemoryMiB < 0 {
			return fmt.Errorf("%s: RAM must not be negative", role)
		}
	}
	return nil
}

// CurrentSize is the size a role's machine template carries today. A zero
// field means the template does not set it, in which case the VM takes the
// value of the Proxmox template it is cloned from (the cilium flavor's machine
// templates, for example, carry no sizing at all).
type CurrentSize struct {
	TemplateName string
	Replicas     int
	NumSockets   int
	NumCores     int
	MemoryMiB    int
	BootGB       int
}

// sizeFromTemplate reads the sizing fields out of a decoded
// ProxmoxMachineTemplate.
func sizeFromTemplate(src map[string]any) CurrentSize {
	var cs CurrentSize
	if meta, ok := nestedMap(src, "metadata"); ok {
		cs.TemplateName, _ = meta["name"].(string)
	}
	spec, ok := nestedMap(src, "spec", "template", "spec")
	if !ok {
		return cs
	}
	cs.NumSockets = jsonInt(spec["numSockets"])
	cs.NumCores = jsonInt(spec["numCores"])
	cs.MemoryMiB = jsonInt(spec["memoryMiB"])
	if boot, ok := nestedMap(spec, "disks", "bootVolume"); ok {
		cs.BootGB = jsonInt(boot["sizeGb"])
	}
	return cs
}

// jsonInt converts a decoded JSON number (float64) or an already-integer value.
func jsonInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// resizedTemplate builds the machine template to create for one role: a copy of
// src with req applied. changed is false when req asks for what src already has
// (including a request for a value src does not set but that equals nothing —
// any explicit value on a field the template leaves unset counts as a change).
//
// The name is derived from a hash of the final spec, so applying the same size
// twice is idempotent (kubectl apply of identical content), and two different
// specs can never share a name — which matters because a machine template is
// immutable and re-applying one name with different content is rejected.
func resizedTemplate(src map[string]any, clusterName, role string, req ResizeRequest) (name string, obj map[string]any, changed bool, err error) {
	srcSpec, ok := nestedMap(src, "spec", "template", "spec")
	if !ok {
		return "", nil, false, fmt.Errorf("machine template has no spec.template.spec")
	}
	// Deep copy: nothing the caller holds may be mutated.
	raw, err := json.Marshal(srcSpec)
	if err != nil {
		return "", nil, false, err
	}
	var spec map[string]any
	if err := json.Unmarshal(raw, &spec); err != nil {
		return "", nil, false, err
	}

	set := func(key string, want int) {
		if want <= 0 {
			return
		}
		if jsonInt(spec[key]) != want {
			changed = true
		}
		spec[key] = want
	}
	set("numSockets", req.NumSockets)
	set("numCores", req.NumCores)
	set("memoryMiB", req.MemoryMiB)
	if !changed {
		return "", nil, false, nil
	}

	// json.Marshal sorts map keys, so the hash is stable.
	canon, err := json.Marshal(spec)
	if err != nil {
		return "", nil, false, err
	}
	sum := sha256.Sum256(canon)
	name = fmt.Sprintf("%s-%s-rs-%x", clusterName, role, sum[:4])

	ns := "default"
	if meta, ok := nestedMap(src, "metadata"); ok {
		if v, _ := meta["namespace"].(string); v != "" {
			ns = v
		}
	}
	obj = map[string]any{
		"apiVersion": src["apiVersion"],
		"kind":       src["kind"],
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec":       map[string]any{"template": map[string]any{"spec": spec}},
	}
	return name, obj, true, nil
}

// getMachineTemplate reads and decodes a ProxmoxMachineTemplate.
func getMachineTemplate(ctx context.Context, kubectlBin, kcPath, name string) (map[string]any, error) {
	raw, err := kubectlOut(ctx, kubectlBin, "--kubeconfig", kcPath,
		"get", "proxmoxmachinetemplate", name, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("reading machine template %s: %w", name, err)
	}
	var src map[string]any
	if err := json.Unmarshal([]byte(raw), &src); err != nil {
		return nil, fmt.Errorf("parsing machine template %s: %w", name, err)
	}
	return src, nil
}

// CurrentSizing reads the live size of the control plane and worker machine
// templates, following the references on the KubeadmControlPlane and
// MachineDeployment (which move on every upgrade or resize).
func CurrentSizing(ctx context.Context, dataDir, binDir, clusterName string) (cp, workers CurrentSize, err error) {
	kubectlBin := filepath.Join(binDir, "kubectl")
	kcPath := bootstrap.KubeconfigPath(dataDir)

	read := func(kind, name, refPath, replicasPath string) (CurrentSize, error) {
		ref, err := machineTemplateRef(ctx, kubectlBin, kcPath, kind, name, refPath)
		if err != nil {
			return CurrentSize{}, err
		}
		src, err := getMachineTemplate(ctx, kubectlBin, kcPath, ref)
		if err != nil {
			return CurrentSize{}, err
		}
		cs := sizeFromTemplate(src)
		if cs.TemplateName == "" {
			cs.TemplateName = ref
		}
		if out, err := kubectlOut(ctx, kubectlBin, "--kubeconfig", kcPath,
			"get", kind, name, "-o", "jsonpath="+replicasPath); err == nil {
			fmt.Sscanf(strings.TrimSpace(out), "%d", &cs.Replicas)
		}
		return cs, nil
	}

	cp, err = read("kubeadmcontrolplane", clusterName+"-control-plane",
		"{.spec.machineTemplate.spec.infrastructureRef.name}", "{.spec.replicas}")
	if err != nil {
		return cp, workers, err
	}
	workers, err = read("machinedeployment", clusterName+"-workers",
		"{.spec.template.spec.infrastructureRef.name}", "{.spec.replicas}")
	return cp, workers, err
}

// applyObject creates or updates one object with `kubectl apply`.
func applyObject(c *jobs.Ctx, kubectlBin, kcPath string, obj map[string]any) error {
	body, err := json.Marshal(obj)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPath, "apply", "-f", "-")
	cmd.Stdin = bytes.NewReader(body)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w\n%s", err, out.String())
	}
	return nil
}

// ResizeSpec performs the whole resize as one job: create the new machine
// templates, repoint the control plane and/or the workers at them, then watch
// Cluster API replace the machines.
//
// The control plane is repointed first, matching the upgrade order: it keeps
// the API available while the workers roll behind it.
func ResizeSpec(dataDir, binDir string, in ResizeInput) *jobs.Spec {
	kcpName := in.ClusterName + "-control-plane"
	mdName := in.ClusterName + "-workers"

	title := "Resize " + in.ClusterName
	var what []string
	if !in.ControlPlane.IsZero() {
		what = append(what, "control plane: "+in.ControlPlane.Describe())
	}
	if !in.Workers.IsZero() {
		what = append(what, "workers: "+in.Workers.Describe())
	}
	if len(what) > 0 {
		title += " (" + strings.Join(what, "; ") + ")"
	}

	// Filled by the first step, read by the next two.
	var cpNew, workerNew string

	return jobs.NewSpec("cluster.resize", title).
		Step("Create machine templates with the new size", func(c *jobs.Ctx) error {
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)

			create := func(role, kind, name, refPath string, req ResizeRequest) (string, error) {
				if req.IsZero() {
					return "", nil
				}
				srcName, err := machineTemplateRef(c, kubectlBin, kcPath, kind, name, refPath)
				if err != nil {
					return "", err
				}
				src, err := getMachineTemplate(c, kubectlBin, kcPath, srcName)
				if err != nil {
					return "", err
				}
				cur := sizeFromTemplate(src)
				c.Logf("%s machine template %s today: %d socket(s) × %d core(s), %s RAM (0 = taken from the Proxmox template)",
					role, srcName, cur.NumSockets, cur.NumCores, FormatMiB(cur.MemoryMiB))

				newName, obj, changed, err := resizedTemplate(src, in.ClusterName, role, req)
				if err != nil {
					return "", err
				}
				if !changed {
					c.Logf("%s already has the requested size — leaving it alone.", role)
					return "", nil
				}
				if err := applyObject(c, kubectlBin, kcPath, obj); err != nil {
					return "", fmt.Errorf("creating machine template %s: %w", newName, err)
				}
				c.Logf("Machine template %s created (cloned from %s, %s)", newName, srcName, req.Describe())
				return newName, nil
			}

			var err error
			if cpNew, err = create("control-plane", "kubeadmcontrolplane", kcpName,
				"{.spec.machineTemplate.spec.infrastructureRef.name}", in.ControlPlane); err != nil {
				return err
			}
			if workerNew, err = create("worker", "machinedeployment", mdName,
				"{.spec.template.spec.infrastructureRef.name}", in.Workers); err != nil {
				return err
			}
			if cpNew == "" && workerNew == "" {
				return fmt.Errorf("nothing to change: the requested sizes already match the cluster's machine templates")
			}
			return nil
		}).
		Step("Roll the control plane onto the new size", func(c *jobs.Ctx) error {
			if cpNew == "" {
				c.Logf("Control plane unchanged.")
				return nil
			}
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)
			patch := fmt.Sprintf(`{"spec":{"machineTemplate":{"spec":{"infrastructureRef":{"name":%q}}}}}`, cpNew)
			if err := runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
				"patch", "kubeadmcontrolplane", kcpName, "--type=merge", "-p", patch); err != nil {
				return err
			}
			c.Logf("Cluster API will now replace the control plane machine(s) one at a time — each is a fresh Proxmox VM.")
			return nil
		}).
		Step("Roll the workers onto the new size", func(c *jobs.Ctx) error {
			if workerNew == "" {
				c.Logf("Workers unchanged.")
				return nil
			}
			kubectlBin := filepath.Join(binDir, "kubectl")
			kcPath := bootstrap.KubeconfigPath(dataDir)
			patch := fmt.Sprintf(`{"spec":{"template":{"spec":{"infrastructureRef":{"name":%q}}}}}`, workerNew)
			return runner.Run(c, c, "", nil, kubectlBin, "--kubeconfig", kcPath,
				"patch", "machinedeployment", mdName, "--type=merge", "-p", patch)
		}).
		Step("Wait for the rolling replacement to finish", waitForRollout(dataDir, binDir, in.ClusterName, "Resize", "the new machine size"))
}
