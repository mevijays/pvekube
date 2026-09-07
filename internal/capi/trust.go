// Internal CA trust for workloads running *inside* the cluster.
//
// Trusting an internal CA is three separate problems wearing one name, and
// solving one solves neither of the others:
//
//  1. containerd, so kubelet can pull images from an internal registry.
//     Handled by RegistryConfig's /etc/containerd/certs.d entry
//     (registry.go) — node-level, delivered by the bootstrap manifest.
//  2. The node's own OS trust store, so anything running on the host
//     (curl, crictl, a kubelet plugin) verifies internal TLS. Handled by
//     RegistryConfig.systemCertPath(), rehomed for Flatcar by
//     flatcarTrustAnchorPath (ignition.go).
//  3. Pods. Not handled by either of the above, and this is the gap this
//     file exists to close.
//
// A container does not see the node's filesystem. It verifies TLS against
// the CA bundle baked into its own image, so a pod calling an internal
// HTTPS endpoint — an artifact repository, an internal portal, an OIDC
// issuer, a Git server — fails with "x509: certificate signed by unknown
// authority" no matter how thoroughly the node trusts that CA. Verified on
// a live v1.36.2 cluster: nothing in any namespace held a copy of the
// operator's CA, so no workload could have verified any internal endpoint.
//
// Kubernetes does have a native answer, ClusterTrustBundle plus a
// clusterTrustBundle projected volume, but it is feature-gated off by
// default and `kubectl api-resources` on that same v1.36.2 cluster returns
// nothing for it. So the portable mechanism is what this uses: publish the
// CA as a ConfigMap the workload mounts.
//
// The honest limitation, stated here rather than discovered later: a
// ConfigMap is namespaced, so this publishes one copy per namespace that
// exists when it runs, and a namespace created afterwards will not have it
// until the next apply. Keeping it continuously in sync across future
// namespaces is a controller's job — cert-manager's trust-manager is the
// usual choice and can consume this same ConfigMap as its source.
package capi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"pvekube/internal/jobs"
)

const (
	// InternalCAConfigMapName is stable and identical in every namespace,
	// so a Deployment's volume definition is portable across namespaces and
	// re-running this step updates rather than accumulates.
	InternalCAConfigMapName = "pvekube-internal-ca"
	// InternalCAKey matches the convention Kubernetes itself uses for the
	// kube-root-ca.crt ConfigMap, so mount snippets look familiar.
	InternalCAKey = "ca.crt"
)

// DistributeInternalCAStep publishes the operator's internal CA as a
// ConfigMap in every existing namespace so pods can mount and trust it.
//
// Deliberately additive: it publishes the CA and never rewrites a
// workload's own trust configuration. Replacing a container's
// ca-certificates.crt wholesale would make internal TLS work and every
// public TLS call fail, so the mount is left to the workload, which knows
// whether it wants the CA appended (SSL_CERT_FILE, NODE_EXTRA_CA_CERTS,
// REQUESTS_CA_BUNDLE, GIT_SSL_CAINFO) or handed to a specific client.
func DistributeInternalCAStep(dataDir, binDir, clusterName, caPEM string) func(*jobs.Ctx) error {
	return func(c *jobs.Ctx) error {
		ca := strings.TrimSpace(caPEM)
		if ca == "" {
			c.Logf("No internal CA certificate configured — nothing to publish for workloads")
			return nil
		}
		kcPath, cleanup, err := waitForWorkloadKubeconfig(c, dataDir, binDir, clusterName, "publishing the internal CA")
		if err != nil {
			return err
		}
		defer cleanup()

		kubectlBin := filepath.Join(binDir, "kubectl")
		out, err := kubectlOut(c, kubectlBin, "--kubeconfig", kcPath,
			"get", "namespaces", "-o", "jsonpath={.items[*].metadata.name}")
		if err != nil {
			return fmt.Errorf("listing namespaces to publish the internal CA into: %w", err)
		}
		namespaces := strings.Fields(out)
		if len(namespaces) == 0 {
			return fmt.Errorf("the cluster reported no namespaces, which cannot be right — refusing to continue")
		}

		manifest, err := internalCAConfigMaps(namespaces, ca+"\n")
		if err != nil {
			return err
		}
		applyCmd := exec.CommandContext(c, kubectlBin, "--kubeconfig", kcPath, "apply", "-f", "-")
		applyCmd.Stdin = strings.NewReader(manifest)
		var applyOut bytes.Buffer
		applyCmd.Stdout, applyCmd.Stderr = &applyOut, &applyOut
		if err := applyCmd.Run(); err != nil {
			return fmt.Errorf("publishing the internal CA ConfigMap: %w\n%s", err, applyOut.String())
		}

		c.Logf("Internal CA published as ConfigMap %q (key %s) in %d namespace(s): %s",
			InternalCAConfigMapName, InternalCAKey, len(namespaces), strings.Join(namespaces, ", "))
		c.Logf("Pods do not pick this up automatically — mount it where the workload expects a CA, for example:")
		c.Logf("    volumes:   [{name: internal-ca, configMap: {name: %s}}]", InternalCAConfigMapName)
		c.Logf("    volumeMounts: [{name: internal-ca, mountPath: /etc/pvekube-ca, readOnly: true}]")
		c.Logf("    env:       [{name: SSL_CERT_FILE, value: /etc/pvekube-ca/%s}]", InternalCAKey)
		c.Logf("A namespace created after this point will not have the ConfigMap until the next apply.")
		return nil
	}
}

// internalCAConfigMaps renders one ConfigMap per namespace as a multi-document
// manifest. Built with encoding/json (valid YAML) rather than string
// concatenation so a PEM body can never break the document structure.
func internalCAConfigMaps(namespaces []string, caPEM string) (string, error) {
	var b strings.Builder
	for _, ns := range namespaces {
		cm := map[string]any{
			"apiVersion": "v1",
			"kind":       "ConfigMap",
			"metadata": map[string]any{
				"name":      InternalCAConfigMapName,
				"namespace": ns,
				"labels": map[string]string{
					"app.kubernetes.io/managed-by": "pvekube",
				},
				"annotations": map[string]string{
					"pvekube.io/description": "Internal CA certificate. Mount this to let a pod verify TLS against internal HTTPS endpoints.",
				},
			},
			"data": map[string]string{InternalCAKey: caPEM},
		}
		enc, err := json.Marshal(cm)
		if err != nil {
			return "", fmt.Errorf("rendering the internal CA ConfigMap for namespace %q: %w", ns, err)
		}
		b.WriteString("---\n")
		b.Write(enc)
		b.WriteString("\n")
	}
	return b.String(), nil
}

// internalCASystemPath is where the CA lands in the node's OS trust store.
// Deliberately the same cloud-init trust directory RegistryConfig uses, so
// dropUsrFiles' Flatcar rewrite (ignition.go) picks it up for free rather
// than needing a second special case.
const internalCASystemPath = "/usr/local/share/ca-certificates/pvekube-internal-ca.crt"

// internalCAScriptPath refreshes the OS bundle after the file is written.
// Needed on its own because a cluster may configure an internal CA without
// configuring a registry, in which case configure-registry.sh — which also
// runs update-ca-certificates — is never written at all.
const internalCAScriptPath = "/etc/pvekube/install-internal-ca.sh"

const internalCAScript = `#!/bin/bash
# Written by PVEKube. Adds the operator's internal CA to this node's OS trust
# store, so anything on the host verifying TLS against an internal HTTPS
# endpoint succeeds. Pods do NOT inherit this — they get the same CA from the
# pvekube-internal-ca ConfigMap instead.
set -euo pipefail
update-ca-certificates
`

// InjectInternalCATrust adds the operator's internal CA to every node's OS
// trust store, independently of whether a private registry was configured.
//
// Same decode-find-mutate-reencode pattern as InjectRegistryTrust, and must
// run before InjectIgnitionFormat for the same reason that one does: the
// Flatcar pass rewrites /usr paths, so this has to have already written them.
func InjectInternalCATrust(manifestYAML, caPEM string) (string, error) {
	ca := strings.TrimSpace(caPEM)
	if ca == "" {
		return manifestYAML, nil
	}
	files := []cloudInitFile{
		{Path: internalCASystemPath, Owner: "root:root", Permissions: "0644", Content: ca + "\n"},
		{Path: internalCAScriptPath, Owner: "root:root", Permissions: "0755", Content: internalCAScript},
	}
	return injectBootstrapFiles(manifestYAML, files, internalCAScriptPath,
		"internal CA trust requested but the generated manifest had no KubeadmControlPlane or KubeadmConfigTemplate to inject into")
}
