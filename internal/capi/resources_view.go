// Resource viewer data: everything the /clusters/{name}/resources page needs
// to render its AG Grid tables and Chart.js summaries, fetched from the
// workload cluster in one pass and handed to the frontend as JSON. Namespace
// and text filtering both happen client-side against this one payload —
// simpler and far more responsive than round-tripping per filter change,
// and completely fine at the scale a homelab cluster actually reaches.
package capi

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
)

// resourceKinds is what one combined `kubectl get` call fetches. Order
// doesn't matter — items come back tagged with their own kind and get
// regrouped below.
var resourceKinds = []string{
	"deployments", "statefulsets", "daemonsets", "pods",
	"configmaps", "secrets",
	"persistentvolumeclaims", "persistentvolumes", "storageclasses",
}

type NodeView struct {
	Name             string `json:"name"`
	Ready            bool   `json:"ready"`
	CPUCapacityM     int64  `json:"cpuCapacityM"`
	MemCapacityKi    int64  `json:"memCapacityKi"`
	CPUAllocatableM  int64  `json:"cpuAllocatableM"`
	MemAllocatableKi int64  `json:"memAllocatableKi"`
	CPUUsageM        *int64 `json:"cpuUsageM,omitempty"`
	MemUsageKi       *int64 `json:"memUsageKi,omitempty"`
}

// ResourcesView is the full JSON payload the resource viewer page fetches.
type ResourcesView struct {
	Namespaces       []string         `json:"namespaces"`
	Nodes            []NodeView       `json:"nodes"`
	Resources        map[string][]any `json:"resources"` // kind (plural, matches resourceKinds) -> raw kubectl items, each with computed "_cpuRequestM" etc. appended for pods
	MetricsAvailable bool             `json:"metricsAvailable"`
}

// GetResourcesView fetches everything in one pass: nodes, the combined
// multi-kind resource list, and (best-effort — absent if metrics-server
// isn't installed) live usage via `kubectl top`.
func GetResourcesView(ctx context.Context, dataDir, binDir, clusterName string) (*ResourcesView, error) {
	kcPath, cleanup, err := workloadKubeconfigFile(ctx, dataDir, binDir, clusterName)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	kubectlBin := filepath.Join(binDir, "kubectl")
	view := &ResourcesView{Resources: map[string][]any{}}

	nodeItems, err := kubectlGetItems(ctx, kubectlBin, kcPath, "nodes", false)
	if err != nil {
		return nil, fmt.Errorf("listing nodes: %w", err)
	}

	usageByNode := map[string][2]int64{}    // name -> [cpuM, memKi]
	usageByPodKey := map[string][2]int64{}  // "ns/name" -> [cpuM, memKi]
	if out, err := exec.CommandContext(ctx, kubectlBin, "--kubeconfig", kcPath, "top", "nodes", "--no-headers").Output(); err == nil {
		view.MetricsAvailable = true
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			f := strings.Fields(line)
			if len(f) < 5 {
				continue
			}
			cpu := parseQuantity(f[1]).MilliValue()
			mem := parseQuantity(f[3]).Value() / 1024
			usageByNode[f[0]] = [2]int64{cpu, mem}
		}
	}
	if out, err := exec.CommandContext(ctx, kubectlBin, "--kubeconfig", kcPath, "top", "pods", "-A", "--no-headers").Output(); err == nil {
		view.MetricsAvailable = true
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			f := strings.Fields(line)
			if len(f) < 4 {
				continue
			}
			cpu := parseQuantity(f[2]).MilliValue()
			mem := parseQuantity(f[3]).Value() / 1024
			usageByPodKey[f[0]+"/"+f[1]] = [2]int64{cpu, mem}
		}
	}

	for _, n := range nodeItems {
		nm, _ := n["metadata"].(map[string]any)
		name, _ := nm["name"].(string)
		status, _ := n["status"].(map[string]any)
		capacity, _ := status["capacity"].(map[string]any)
		allocatable, _ := status["allocatable"].(map[string]any)

		nv := NodeView{
			Name:             name,
			Ready:            nodeIsReady(status),
			CPUCapacityM:     quantityM(capacity, "cpu"),
			MemCapacityKi:    quantityKi(capacity, "memory"),
			CPUAllocatableM:  quantityM(allocatable, "cpu"),
			MemAllocatableKi: quantityKi(allocatable, "memory"),
		}
		if u, ok := usageByNode[name]; ok {
			cpu, mem := u[0], u[1]
			nv.CPUUsageM, nv.MemUsageKi = &cpu, &mem
		}
		view.Nodes = append(view.Nodes, nv)
	}

	combined, err := kubectlGetItems(ctx, kubectlBin, kcPath, strings.Join(resourceKinds, ","), true)
	if err != nil {
		return nil, fmt.Errorf("listing resources: %w", err)
	}

	nsSet := map[string]bool{}
	for _, item := range combined {
		kind, _ := item["kind"].(string)
		plural := kindToPlural(kind)
		if plural == "" {
			continue
		}
		if md, ok := item["metadata"].(map[string]any); ok {
			if ns, ok := md["namespace"].(string); ok && ns != "" {
				nsSet[ns] = true
			}
		}
		if plural == "pods" {
			annotatePodResources(item, usageByPodKey)
		}
		view.Resources[plural] = append(view.Resources[plural], item)
	}
	for ns := range nsSet {
		view.Namespaces = append(view.Namespaces, ns)
	}
	sortStrings(view.Namespaces)

	return view, nil
}

// workloadKubeconfigFile is a non-waiting variant of waitForWorkloadKubeconfig
// for read-only viewing — the resource viewer should fail fast with a clear
// "cluster not reachable yet" error rather than block for 15 minutes like an
// install step legitimately should.
func workloadKubeconfigFile(ctx context.Context, dataDir, binDir, clusterName string) (path string, cleanup func(), err error) {
	kc, err := GetWorkloadKubeconfig(ctx, dataDir, binDir, clusterName)
	if err != nil {
		return "", nil, fmt.Errorf("cluster not reachable yet (its API may still be provisioning): %w", err)
	}
	return writeTempKubeconfig(dataDir, kc)
}

func kubectlGetItems(ctx context.Context, kubectlBin, kcPath, resources string, allNamespaces bool) ([]map[string]any, error) {
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	args := []string{"--kubeconfig", kcPath, "get", resources, "-o", "json"}
	if allNamespaces {
		args = append(args, "-A")
	}
	out, err := exec.CommandContext(cctx, kubectlBin, args...).Output()
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(out, &list); err != nil {
		return nil, fmt.Errorf("parsing kubectl output: %w", err)
	}
	return list.Items, nil
}

func nodeIsReady(status map[string]any) bool {
	conds, _ := status["conditions"].([]any)
	for _, c := range conds {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := cm["type"].(string); t == "Ready" {
			s, _ := cm["status"].(string)
			return s == "True"
		}
	}
	return false
}

func quantityM(m map[string]any, key string) int64 {
	s, _ := m[key].(string)
	return parseQuantity(s).MilliValue()
}

func quantityKi(m map[string]any, key string) int64 {
	s, _ := m[key].(string)
	return parseQuantity(s).Value() / 1024
}

func parseQuantity(s string) *resource.Quantity {
	if s == "" {
		return &resource.Quantity{}
	}
	q, err := resource.ParseQuantity(s)
	if err != nil {
		return &resource.Quantity{}
	}
	return &q
}

// annotatePodResources sums a pod's container requests/limits and attaches
// them (plus any live usage) as extra fields on the raw item — the frontend
// reads these directly rather than re-implementing k8s quantity math in JS.
func annotatePodResources(item map[string]any, usageByPodKey map[string][2]int64) {
	var cpuReq, memReq, cpuLim, memLim resource.Quantity
	spec, _ := item["spec"].(map[string]any)
	containers, _ := spec["containers"].([]any)
	for _, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		res, _ := cm["resources"].(map[string]any)
		if reqs, ok := res["requests"].(map[string]any); ok {
			if v, ok := reqs["cpu"].(string); ok {
				cpuReq.Add(*parseQuantity(v))
			}
			if v, ok := reqs["memory"].(string); ok {
				memReq.Add(*parseQuantity(v))
			}
		}
		if lims, ok := res["limits"].(map[string]any); ok {
			if v, ok := lims["cpu"].(string); ok {
				cpuLim.Add(*parseQuantity(v))
			}
			if v, ok := lims["memory"].(string); ok {
				memLim.Add(*parseQuantity(v))
			}
		}
	}
	item["_cpuRequestM"] = cpuReq.MilliValue()
	item["_memRequestKi"] = memReq.Value() / 1024
	item["_cpuLimitM"] = cpuLim.MilliValue()
	item["_memLimitKi"] = memLim.Value() / 1024

	md, _ := item["metadata"].(map[string]any)
	ns, _ := md["namespace"].(string)
	name, _ := md["name"].(string)
	if u, ok := usageByPodKey[ns+"/"+name]; ok {
		item["_cpuUsageM"] = u[0]
		item["_memUsageKi"] = u[1]
	}
}

func kindToPlural(kind string) string {
	switch kind {
	case "Deployment":
		return "deployments"
	case "StatefulSet":
		return "statefulsets"
	case "DaemonSet":
		return "daemonsets"
	case "Pod":
		return "pods"
	case "ConfigMap":
		return "configmaps"
	case "Secret":
		return "secrets"
	case "PersistentVolumeClaim":
		return "persistentvolumeclaims"
	case "PersistentVolume":
		return "persistentvolumes"
	case "StorageClass":
		return "storageclasses"
	default:
		return ""
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}
