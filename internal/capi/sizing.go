// Machine sizing, per role.
//
// `clusterctl generate cluster` fills CAPMOX's cluster-template.yaml from ONE
// set of variables (NUM_SOCKETS, NUM_CORES, MEMORY_MIB, BOOT_VOLUME_SIZE), and
// the default template applies them to both the control-plane and the worker
// ProxmoxMachineTemplate — there is no per-role variable to set. Worse, not
// every flavor consumes them: the cilium template's machine templates have no
// numSockets/numCores/memoryMiB/disks fields at all, so the variables vanish
// and every VM comes out as big as the source template, whatever the form said.
//
// The two templates are separate objects, so the fix for both problems is the
// same: after generation, write the size into each template explicitly.
package capi

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// MachineSizing is the size of one role's machines.
type MachineSizing struct {
	NumSockets       int
	NumCores         int
	MemoryMiB        int
	BootVolumeSizeGB int
}

// WorkerSizing overrides the worker nodes' machine size. A zero field keeps
// the control plane's value, so the zero value is "workers look exactly like
// the control plane".
type WorkerSizing struct {
	NumSockets       int
	NumCores         int
	MemoryMiB        int
	BootVolumeSizeGB int
}

// IsZero reports whether no override was requested.
func (w WorkerSizing) IsZero() bool { return w == WorkerSizing{} }

// Over returns the worker size: the override where given, else cp's value.
func (w WorkerSizing) Over(cp MachineSizing) MachineSizing {
	pick := func(override, inherited int) int {
		if override > 0 {
			return override
		}
		return inherited
	}
	return MachineSizing{
		NumSockets:       pick(w.NumSockets, cp.NumSockets),
		NumCores:         pick(w.NumCores, cp.NumCores),
		MemoryMiB:        pick(w.MemoryMiB, cp.MemoryMiB),
		BootVolumeSizeGB: pick(w.BootVolumeSizeGB, cp.BootVolumeSizeGB),
	}
}

// InjectMachineSizing writes cp into the control-plane ProxmoxMachineTemplate
// and cp-with-the-worker-override into the worker one, adding any field the
// flavor left out. The templates are found through the KubeadmControlPlane's
// and the MachineDeployment's infrastructureRef rather than by name, so it
// keeps working if upstream renames them. bootDevice (e.g. "scsi0") is used
// only where a template has a boot volume without a disk; an existing disk is
// kept, because Proxmox requires the field and it is the clone's real device.
//
// Same decode-find-mutate-reencode pattern as InjectCredentialsRef: yaml.Node
// preserves every untouched document as it was, which matters because this
// manifest is shown on the preview screen and stored with the cluster.
func InjectMachineSizing(manifestYAML string, cp MachineSizing, w WorkerSizing, bootDevice string) (string, error) {
	if bootDevice == "" {
		bootDevice = "scsi0"
	}

	dec := yaml.NewDecoder(strings.NewReader(manifestYAML))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if err.Error() == "EOF" {
				break
			}
			return "", fmt.Errorf("parsing generated manifest: %w", err)
		}
		docs = append(docs, &doc)
	}

	cpTemplate, workerTemplate := "", ""
	for _, doc := range docs {
		root := docRoot(doc)
		if root == nil {
			continue
		}
		kind := mapValueNode(root, "kind")
		if kind == nil {
			continue
		}
		switch kind.Value {
		case "KubeadmControlPlane":
			mt := mapValueNode(mapValueNode(root, "spec"), "machineTemplate")
			// The reference sits under machineTemplate.spec in the v1beta2
			// API and directly under machineTemplate in older ones.
			ref := mapValueNode(mapValueNode(mt, "spec"), "infrastructureRef")
			if ref == nil {
				ref = mapValueNode(mt, "infrastructureRef")
			}
			if n := mapValueNode(ref, "name"); n != nil && cpTemplate == "" {
				cpTemplate = n.Value
			}
		case "MachineDeployment":
			ref := mapValueNode(mapValueNode(mapValueNode(mapValueNode(root, "spec"), "template"), "spec"), "infrastructureRef")
			if n := mapValueNode(ref, "name"); n != nil && workerTemplate == "" {
				workerTemplate = n.Value
			}
		}
	}
	if cpTemplate == "" || workerTemplate == "" {
		return "", fmt.Errorf("cannot apply machine sizing: the generated manifest's KubeadmControlPlane or MachineDeployment has no infrastructureRef (control plane %q, workers %q)", cpTemplate, workerTemplate)
	}

	sizes := map[string]MachineSizing{cpTemplate: cp, workerTemplate: w.Over(cp)}
	patched := 0
	for _, doc := range docs {
		root := docRoot(doc)
		if root == nil {
			continue
		}
		if k := mapValueNode(root, "kind"); k == nil || k.Value != "ProxmoxMachineTemplate" {
			continue
		}
		n := mapValueNode(mapValueNode(root, "metadata"), "name")
		if n == nil {
			continue
		}
		size, ok := sizes[n.Value]
		if !ok {
			continue
		}
		spec := infraMachineSpec(doc)
		if spec == nil {
			continue
		}
		setMapValueInt(spec, "numSockets", size.NumSockets)
		setMapValueInt(spec, "numCores", size.NumCores)
		setMapValueInt(spec, "memoryMiB", size.MemoryMiB)
		boot := ensureMapNode(ensureMapNode(spec, "disks"), "bootVolume")
		if mapValueNode(boot, "disk") == nil {
			setMapValue(boot, "disk", bootDevice)
		}
		setMapValueInt(boot, "sizeGb", size.BootVolumeSizeGB)
		patched++
	}
	if patched != len(sizes) {
		return "", fmt.Errorf("cannot apply machine sizing: expected ProxmoxMachineTemplates %q and %q in the generated manifest, found %d of them", cpTemplate, workerTemplate, patched)
	}

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			return "", fmt.Errorf("re-encoding manifest: %w", err)
		}
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("re-encoding manifest: %w", err)
	}
	return out.String(), nil
}

// docRoot unwraps a decoded document to its top-level mapping, or nil.
func docRoot(doc *yaml.Node) *yaml.Node {
	root := doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return nil
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil
	}
	return root
}

// setMapValueInt is setMapValue for integer fields. Encoding these as "!!str"
// would round-trip through the apiserver as the string "8", which the CRD's
// integer schema rejects.
func setMapValueInt(m *yaml.Node, key string, value int) {
	v := strconv.Itoa(value)
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: v}
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: v},
	)
}
