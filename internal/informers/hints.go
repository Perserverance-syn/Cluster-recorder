package informers

import (
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/labels"
)

// Hints is what the collector tells an agent so the agent needs no API
// access: DaemonSets (CNI detection) and the node's pod CIDRs, discovered in
// the spec order: Node.spec.podCIDRs, Node.spec.podCIDR, then the
// kube-controller-manager --cluster-cidr argument. Never guessed.
type Hints struct {
	DaemonSets []string `json:"daemonsets"`
	PodCIDRs   []string `json:"pod_cidrs"`
}

func (c *Collector) Hints(node string) (Hints, error) {
	h := Hints{DaemonSets: []string{}, PodCIDRs: []string{}}
	dss, err := c.daemonsets.List(labels.Everything())
	if err != nil {
		return h, err
	}
	for _, ds := range dss {
		h.DaemonSets = append(h.DaemonSets, ds.Namespace+"/"+ds.Name)
	}
	sort.Strings(h.DaemonSets)

	if n, err := c.nodes.Get(node); err == nil {
		h.PodCIDRs = append(h.PodCIDRs, n.Spec.PodCIDRs...)
		if len(h.PodCIDRs) == 0 && n.Spec.PodCIDR != "" {
			h.PodCIDRs = append(h.PodCIDRs, n.Spec.PodCIDR)
		}
	}
	if cidr := c.clusterCIDR(); cidr != "" {
		h.PodCIDRs = append(h.PodCIDRs, cidr) // routes to peer nodes live in the cluster CIDR, not the node's own
	}
	return h, nil
}

// clusterCIDR reads --cluster-cidr from the kube-controller-manager static
// pod, if the collector can see one. Managed control planes have none.
func (c *Collector) clusterCIDR() string {
	pods, err := c.pods.Pods("kube-system").List(labels.SelectorFromSet(labels.Set{"component": "kube-controller-manager"}))
	if err != nil {
		return ""
	}
	for _, p := range pods {
		for _, ctr := range p.Spec.Containers {
			for _, arg := range append(ctr.Command, ctr.Args...) {
				if v, ok := strings.CutPrefix(arg, "--cluster-cidr="); ok {
					return v
				}
			}
		}
	}
	return ""
}
