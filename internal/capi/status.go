package capi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"

	"pvekube/internal/bootstrap"
)

// Minimal Cluster API object shapes — only the fields PVEKube's status
// screen actually renders. Deliberately not importing client-go/apimachinery
// (a heavy dependency tree) for what's otherwise a few JSON fields; `kubectl
// get -o json` output is a stable, versioned API contract on its own.

type k8sList[T any] struct {
	Items []T `json:"items"`
}

type machineObj struct {
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Spec struct {
		Version string `json:"version"`
	} `json:"spec"`
	Status struct {
		Phase     string `json:"phase"`
		Addresses []struct {
			Type    string `json:"type"`
			Address string `json:"address"`
		} `json:"addresses"`
		NodeRef *struct {
			Name string `json:"name"`
		} `json:"nodeRef"`
	} `json:"status"`
}

type clusterObj struct {
	Status struct {
		Phase               string `json:"phase"`
		ControlPlaneReady   bool   `json:"controlPlaneReady"`
		InfrastructureReady bool   `json:"infrastructureReady"`
		Conditions          []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

type MachineView struct {
	Name       string
	Role       string // "control-plane" or "worker"
	Phase      string
	IP         string
	NodeName   string
	K8sVersion string
}

type ConditionView struct {
	Type, Status, Reason, Message string
}

type ClusterStatus struct {
	Found                bool
	Phase                string
	ControlPlaneReady    bool
	InfrastructureReady  bool
	Conditions           []ConditionView
	Machines             []MachineView
	WorkerReplicas       int // desired, from the MachineDeployment spec (not a machine count)
	ControlPlaneReplicas int // desired, from the KubeadmControlPlane spec
}

// GetStatus queries the management cluster for the Cluster object and all
// Machines belonging to it. Read-only, safe to poll from the UI.
func GetStatus(ctx context.Context, dataDir, binDir, name string) (*ClusterStatus, error) {
	kubectlBin := filepath.Join(binDir, "kubectl")
	kcPath := bootstrap.KubeconfigPath(dataDir)

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	clusterOut, err := exec.CommandContext(cctx, kubectlBin, "--kubeconfig", kcPath, "get", "cluster", name, "-o", "json").Output()
	if err != nil {
		return &ClusterStatus{Found: false}, nil
	}
	var co clusterObj
	if err := json.Unmarshal(clusterOut, &co); err != nil {
		return nil, fmt.Errorf("parsing cluster status: %w", err)
	}

	mctx, mcancel := context.WithTimeout(ctx, 10*time.Second)
	defer mcancel()
	machinesOut, err := exec.CommandContext(mctx, kubectlBin, "--kubeconfig", kcPath, "get", "machines",
		"-l", "cluster.x-k8s.io/cluster-name="+name, "-o", "json").Output()
	if err != nil {
		return nil, fmt.Errorf("listing machines: %w", err)
	}
	var list k8sList[machineObj]
	if err := json.Unmarshal(machinesOut, &list); err != nil {
		return nil, fmt.Errorf("parsing machines: %w", err)
	}

	status := &ClusterStatus{
		Found: true, Phase: co.Status.Phase,
	}
	for _, c := range co.Status.Conditions {
		if c.Type == "ControlPlaneAvailable" && c.Status == "True" {
			status.ControlPlaneReady = true
		}
		if c.Type == "InfrastructureReady" && c.Status == "True" {
			status.InfrastructureReady = true
		}
	}

	if out, err := exec.CommandContext(cctx, kubectlBin, "--kubeconfig", kcPath, "get", "machinedeployment", name+"-workers",
		"-o", "jsonpath={.spec.replicas}").Output(); err == nil {
		fmt.Sscanf(string(out), "%d", &status.WorkerReplicas)
	}
	if out, err := exec.CommandContext(cctx, kubectlBin, "--kubeconfig", kcPath, "get", "kubeadmcontrolplane", name+"-control-plane",
		"-o", "jsonpath={.spec.replicas}").Output(); err == nil {
		fmt.Sscanf(string(out), "%d", &status.ControlPlaneReplicas)
	}
	for _, c := range co.Status.Conditions {
		status.Conditions = append(status.Conditions, ConditionView{c.Type, c.Status, c.Reason, c.Message})
	}
	for _, m := range list.Items {
		role := "worker"
		if _, ok := m.Metadata.Labels["cluster.x-k8s.io/control-plane"]; ok {
			role = "control-plane"
		}
		ip := ""
		for _, a := range m.Status.Addresses {
			if a.Type == "InternalIP" || a.Type == "ExternalIP" {
				ip = a.Address
				break
			}
		}
		nodeName := ""
		if m.Status.NodeRef != nil {
			nodeName = m.Status.NodeRef.Name
		}
		status.Machines = append(status.Machines, MachineView{
			Name: m.Metadata.Name, Role: role, Phase: m.Status.Phase,
			IP: ip, NodeName: nodeName, K8sVersion: m.Spec.Version,
		})
	}
	return status, nil
}

// GetWorkloadKubeconfig fetches the workload cluster's own kubeconfig from
// the Secret Cluster API creates automatically ("<name>-kubeconfig" in the
// same namespace as the Cluster object).
func GetWorkloadKubeconfig(ctx context.Context, dataDir, binDir, name string) ([]byte, error) {
	kubectlBin := filepath.Join(binDir, "kubectl")
	kcPath := bootstrap.KubeconfigPath(dataDir)

	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, kubectlBin, "--kubeconfig", kcPath, "get", "secret", name+"-kubeconfig",
		"-o", "jsonpath={.data.value}").Output()
	if err != nil {
		return nil, fmt.Errorf("fetching kubeconfig secret (cluster may not be far enough along yet): %w", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(string(out))
	if err != nil {
		return nil, fmt.Errorf("decoding kubeconfig secret: %w", err)
	}
	return decoded, nil
}
