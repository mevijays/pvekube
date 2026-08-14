// Ignition bootstrap format for OS flavors that can't run cloud-init.
//
// CABPK's default bootstrap payload is cloud-config: write_files +
// preKubeadmCommands rendered as a cloud-init user-data script. Flatcar
// Container Linux doesn't execute any of that — it only understands
// Ignition. Without this, a Flatcar VM boots, gets an IP, and then does
// nothing: no kube-vip static pod, no registry-trust script, no `kubeadm
// init`. That's silent and easy to misread as a networking problem (the
// control-plane endpoint VIP never comes up because nothing ever ran
// kubeadm to create it) — confirmed live against a stuck "dev" cluster
// whose KubeadmControlPlane had bootstrapDataProvided: true but sat at
// VirtualMachineProvisioned: WaitingForCloudInit indefinitely.
//
// clusterctl's own EXP_KUBEADM_BOOTSTRAP_FORMAT_IGNITION=true feature gate
// (see GenerateInput.env, bootstrap/clusterctl.go's `clusterctl init`) only
// makes CABPK *capable* of emitting Ignition when asked — it doesn't change
// what any single cluster actually gets. Something still has to set
// spec.kubeadmConfigSpec.format / spec.template.spec.format to "ignition"
// per document, which is what this file does, and only for Flatcar: every
// other supported flavor (Ubuntu, Rocky Linux) is cloud-init-capable and
// must keep getting the default format untouched.
//
// Getting Ignition merely accepted wasn't the whole story either — three
// more Ignition-specific gaps surfaced only by watching a real Flatcar
// cluster boot end to end, each fixed here: a transpiler warning CABPK
// escalates to a fatal error (defaultFilePermissions), a write to Flatcar's
// permanently read-only /usr that Ignition treats as boot-halting
// (dropUsrFiles), CAPMOX's own cloud-init-status VM-readiness poll that can
// never succeed without cloud-init (the skipCloudInitStatus checks field),
// and CABPK's provider-id template placeholder never getting resolved
// without cloud-init's templating engine, which leaves every node
// permanently tainted (injectUntaintWorkaround). See each function's doc
// comment for the specific failure this fixes.
//
// The provider-id placeholder issue has a second cost beyond the taint,
// left as a KNOWN, ACCEPTED cosmetic limitation rather than fixed: CAPI's
// own status conditions (ControlPlaneMachinesReady, WorkerMachinesReady,
// the Cluster's phase) key off matching a Machine to its Node by
// providerID, so they stay permanently NotReady/"Provisioned" in PVEKube's
// cluster detail page even once the workload cluster is fully healthy.
// Patching spec.providerID after the fact to fix this was tried and
// confirmed impossible, not just difficult: the Kubernetes API server
// rejects it outright — "node updates may not change providerID except
// from \"\" to valid" — once kubelet has self-registered with any non-empty
// value, which happens immediately at join, before any PVEKube-side step
// could ever intervene. A real fix would mean rewriting kubeadm's own join
// config with the correct value in preKubeadmCommands, before kubeadm ever
// runs, which depends on the VM's local DMI UUID matching CAPMOX's
// providerID — unverified, and out of scope here. Don't re-attempt the
// patch-after-join approach; it cannot work by construction, not just in
// practice.
package capi

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// ignitionOSFlavors is every template OS whose image has no cloud-init and
// therefore requires Ignition-format bootstrap data. Keep in sync with
// imagebuilder.Flavors — currently only Flatcar ships without cloud-init.
var ignitionOSFlavors = map[string]bool{
	"flatcar": true,
}

// needsIgnition reports whether osFlavor requires Ignition-format bootstrap
// data instead of the default cloud-config.
func needsIgnition(osFlavor string) bool {
	return ignitionOSFlavors[strings.ToLower(strings.TrimSpace(osFlavor))]
}

// defaultFilePermissions sets permissions: "0644" (Ignition's own transpiler
// default) on any entry under configSpec.files that omits it.
//
// This isn't cosmetic: CABPK's Ignition path runs the generated files list
// through coreos/container-linux-config-transpiler, which reports a missing
// mode as a WARNING ("mode unspecified for file, defaulting to 0644") — but
// the CABPK version this app pins treats any non-empty transpiler report,
// warnings included, as a fatal error. Confirmed live: a real "dev" cluster
// stuck permanently failing kubeadmconfig reconciliation on exactly this
// warning, traced to CAPMOX's own base cluster-template.yaml, whose kube-vip
// static-pod file entry (files[0], /etc/kubernetes/manifests/kube-vip.yaml)
// has an owner and a path but no permissions key. Since cloud-config
// tolerates a missing mode silently, this only ever surfaces for Ignition
// flavors, and defaulting it here reproduces the exact value ct would have
// applied anyway — this changes nothing about what ends up on disk, only
// stops the transpiler from complaining about it.
func defaultFilePermissions(configSpec *yaml.Node) {
	files := mapValueNode(configSpec, "files")
	if files == nil || files.Kind != yaml.SequenceNode {
		return
	}
	for _, f := range files.Content {
		if f.Kind != yaml.MappingNode {
			continue
		}
		if p := mapValueNode(f, "permissions"); p != nil && p.Tag != "!!null" && p.Value != "" {
			continue
		}
		setMapValue(f, "permissions", "0644")
	}
}

// dropUsrFiles removes any files: entry whose path targets /usr.
//
// Flatcar (and Ignition-based immutable-OS images generally) ships /usr as
// a read-only, dm-verity-protected partition — not just during
// provisioning, permanently. Ignition's files stage treats a failed write
// as CRITICAL and halts boot into an emergency shell rather than
// continuing, confirmed live: a real control-plane VM hung forever with
// "Ignition failed: failed to create files: ... mkdir /sysroot/usr/local/
// share: read-only file system" on its console, having never reached
// kubeadm at all. The offending entry here is
// RegistryConfig.systemCertPath() (registry.go) — it drops the registry's
// CA into /usr/local/share/ca-certificates for the OS-wide trust store,
// which cloud-init-based flavors handle fine. It isn't load-bearing for
// Flatcar: containerd's own pull trust already comes from the sibling
// /etc/containerd/certs.d/.../ca.crt entry, which lives under /etc and is
// writable. Scoped to any /usr path rather than that one specifically, so
// this stays correct if a future file ever targets /usr again.
func dropUsrFiles(configSpec *yaml.Node) {
	files := mapValueNode(configSpec, "files")
	if files == nil || files.Kind != yaml.SequenceNode {
		return
	}
	kept := files.Content[:0]
	for _, f := range files.Content {
		if f.Kind == yaml.MappingNode {
			if p := mapValueNode(f, "path"); p != nil && strings.HasPrefix(p.Value, "/usr") {
				continue
			}
		}
		kept = append(kept, f)
	}
	files.Content = kept
}

// infraMachineSpec returns the mapping at spec.template.spec for a
// ProxmoxMachineTemplate document (same nesting CAPMOX uses for both the
// control-plane and worker machine templates), or nil for any other kind.
func infraMachineSpec(doc *yaml.Node) *yaml.Node {
	root := doc
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) == 0 {
			return nil
		}
		root = root.Content[0]
	}
	kindNode := mapValueNode(root, "kind")
	if kindNode == nil || kindNode.Value != "ProxmoxMachineTemplate" {
		return nil
	}
	spec := mapValueNode(root, "spec")
	return mapValueNode(mapValueNode(spec, "template"), "spec")
}

// setMapValueBool is setMapValue's boolean analog — CAPMOX's
// checks.skipCloudInitStatus is a bool field, and encoding it as a "!!str"
// scalar (what setMapValue does) would round-trip through the apiserver as
// the literal string "true" rather than the boolean, which the CRD schema
// rejects.
func setMapValueBool(m *yaml.Node, key string, value bool) {
	v := "false"
	if value {
		v = "true"
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: v}
			return
		}
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: v},
	)
}

// untaintScriptPath is where the janitor script lands on disk — a real
// script rather than inline shell so it's legible in the manifest preview,
// matching registry.go's registrySetupScriptPath convention.
const untaintScriptPath = "/etc/pvekube/untaint-uninitialized-nodes.sh"

// untaintServicePath is the systemd unit that runs the script in the
// background so postKubeadmCommands can enable it and move on immediately,
// rather than blocking kubeadm's own bootstrap sequence for however long it
// takes workers to join.
const untaintServicePath = "/etc/systemd/system/pvekube-untaint.service"

// untaintScript repeatedly clears the CAPI node.cluster.x-k8s.io/
// uninitialized taint for up to 15 minutes after boot.
//
// This is a workaround for a real CABPK/Ignition gap, not a PVEKube bug:
// CAPMOX's own cluster-template.yaml sets kubeletExtraArgs.provider-id to
// "proxmox://{{ ds.meta_data.instance_id }}" — a cloud-init Jinja
// placeholder that only cloud-init's own templating engine resolves.
// Ignition has no equivalent substitution step, so CABPK's Ignition
// renderer passes the literal, unresolved string straight through; kubelet
// ends up registering with spec.providerID literally set to that string.
// CAPI's node controller can then never match this Machine to its Node (it
// correlates by providerID), so the node.cluster.x-k8s.io/uninitialized
// taint kubelet registers with — added specifically so an unmatched, prove-
// yourself-legitimate node can't schedule real workloads — is never
// removed. Confirmed live: a fully healthy 4-node Flatcar cluster (every
// node Ready, kube-vip/calico/coredns/kube-apiserver all Running) had every
// non-DaemonSet pod (istiod, metrics-server, the ingress gateway) stuck
// Pending forever, invisible unless you went looking at spec.taints, since
// DaemonSets tolerate this taint by default and gave no hint anything was
// wrong.
//
// Only the control-plane node has the credentials (/etc/kubernetes/
// admin.conf) to remove another node's taint — on workers this exits
// immediately as a no-op, which is why it's safe to inject identically into
// both KubeadmControlPlane and KubeadmConfigTemplate rather than needing to
// tell them apart.
func untaintScript() string {
	return `#!/bin/bash
# Written by PVEKube — see ignition.go's untaintScript doc comment for why
# this exists. Only does real work on the control-plane node.
set -uo pipefail

ADMIN_CONF=/etc/kubernetes/admin.conf
[ -f "$ADMIN_CONF" ] || exit 0

KUBECTL=""
for candidate in /opt/bin/kubectl /usr/local/bin/kubectl /usr/bin/kubectl; do
  if [ -x "$candidate" ]; then KUBECTL="$candidate"; break; fi
done
if [ -z "$KUBECTL" ]; then KUBECTL="$(command -v kubectl || true)"; fi
[ -n "$KUBECTL" ] || exit 0

deadline=$(( $(date +%s) + 900 ))
while [ "$(date +%s)" -lt "$deadline" ]; do
  tainted=$("$KUBECTL" --kubeconfig="$ADMIN_CONF" get nodes \
    -o jsonpath='{range .items[*]}{.metadata.name}{"="}{range .spec.taints[*]}{.key}{","}{end}{"\n"}{end}' \
    2>/dev/null | awk -F'=' '$2 ~ /node\.cluster\.x-k8s\.io\/uninitialized/ {print $1}')
  for node in $tainted; do
    "$KUBECTL" --kubeconfig="$ADMIN_CONF" taint node "$node" node.cluster.x-k8s.io/uninitialized- >/dev/null 2>&1 || true
  done
  sleep 10
done
`
}

func untaintServiceUnit() string {
	return `[Unit]
Description=PVEKube: clear the CAPI uninitialized taint (works around a CABPK/Ignition provider-id gap on Flatcar)
After=kubelet.service

[Service]
Type=simple
ExecStart=` + untaintScriptPath + `
Restart=no

[Install]
WantedBy=multi-user.target
`
}

// injectUntaintWorkaround adds the janitor script/unit to configSpec's files
// and enables it via postKubeadmCommands, using the exact same
// cloudInitFile/literalContentNode/ensureSeqNode machinery InjectRegistryTrust
// uses in registry.go.
func injectUntaintWorkaround(configSpec *yaml.Node) error {
	files := []cloudInitFile{
		{Path: untaintScriptPath, Owner: "root:root", Permissions: "0755", Content: untaintScript()},
		{Path: untaintServicePath, Owner: "root:root", Permissions: "0644", Content: untaintServiceUnit()},
	}
	filesNode := ensureSeqNode(configSpec, "files")
	for _, f := range files {
		n, err := literalContentNode(f)
		if err != nil {
			return err
		}
		filesNode.Content = append(filesNode.Content, n)
	}
	cmdsNode := ensureSeqNode(configSpec, "postKubeadmCommands")
	cmdsNode.Content = append(cmdsNode.Content, &yaml.Node{
		Kind: yaml.ScalarNode, Tag: "!!str",
		Value: "systemctl daemon-reload && systemctl enable --now pvekube-untaint.service",
	})
	return nil
}

// InjectIgnitionFormat sets format: ignition on both KubeadmControlPlane's
// kubeadmConfigSpec and KubeadmConfigTemplate's template.spec when osFlavor
// needs it, so control-plane and worker nodes alike get Ignition instead of
// cloud-config. A no-op (byte-identical output) for every other flavor.
//
// This only flips the encoding CABPK renders the bootstrap data in — the
// same files/preKubeadmCommands/users fields already built by
// InjectRegistryTrust and clusterctl's own template get translated into
// Ignition units automatically, no restructuring needed.
//
// It also sets spec.template.spec.checks.skipCloudInitStatus: true on both
// ProxmoxMachineTemplate documents (control-plane and worker). This is
// CAPMOX's own documented workaround (docs/Usage.md's Flatcar section) for
// a real, separate failure mode: CAPMOX's VM-readiness poll runs `cloud-init
// status` over the QEMU guest agent, which errors "no pid returned from
// agent exec command" forever on an Ignition-provisioned image — Flatcar
// has no cloud-init binary to run. Confirmed live: a fully healthy 4-node
// "dev" cluster (all nodes Ready, kube-vip/calico/coredns/kube-apiserver
// all Running) stuck with every ProxmoxMachine.status.ready permanently
// false, which meant CAPI never removed the node.cluster.x-k8s.io/
// uninitialized taint from any worker — silently blocking every pod that
// doesn't tolerate it (istiod, metrics-server, the ingress gateway) from
// ever being scheduled, anywhere, while DaemonSets (calico-node, kube-proxy)
// kept working fine and gave no hint anything was wrong.
func InjectIgnitionFormat(manifestYAML, osFlavor string) (string, error) {
	if !needsIgnition(osFlavor) {
		return manifestYAML, nil
	}

	dec := yaml.NewDecoder(strings.NewReader(manifestYAML))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			return "", fmt.Errorf("parsing generated manifest: %w", err)
		}
		docs = append(docs, &doc)
	}

	patched := 0
	for _, doc := range docs {
		if configSpec := bootstrapConfigSpec(doc); configSpec != nil {
			setMapValue(configSpec, "format", "ignition")
			dropUsrFiles(configSpec)
			if err := injectUntaintWorkaround(configSpec); err != nil {
				return "", err
			}
			defaultFilePermissions(configSpec)
			patched++
			continue
		}
		if machineSpec := infraMachineSpec(doc); machineSpec != nil {
			checks := ensureMapNode(machineSpec, "checks")
			setMapValueBool(checks, "skipCloudInitStatus", true)
			patched++
		}
	}
	if patched == 0 {
		return "", fmt.Errorf("ignition format requested but the generated manifest had no KubeadmControlPlane, KubeadmConfigTemplate, or ProxmoxMachineTemplate to inject into")
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
