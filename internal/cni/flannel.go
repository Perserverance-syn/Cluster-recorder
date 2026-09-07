package cni

import (
	"context"

	"github.com/Perserverance-syn/Cluster-recorder/internal/netlink"
	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
)

// Flannel: pod_bridge = cni0, overlay_tunnel = flannel.1 (VXLAN backend) or
// not_applicable (host-gw backend, where peer routes go via node IPs on the
// physical NIC).
type Flannel struct {
	mode string // sticky: "vxlan" once flannel.1 has been seen
}

func (*Flannel) Name() string { return "flannel" }

func (*Flannel) Detect(ctx DetectContext) (int, string) {
	return score(hasAny(pluginTypes(ctx), "flannel"),
		hasAny(dsNames(ctx), "kube-flannel-ds", "kube-flannel"),
		hasAny(ctx.Interfaces, "flannel.1") || hasAny(ctx.Interfaces, "cni0"), "flannel")
}

func (f *Flannel) Topology(v *netlink.View) Topology {
	if f.mode == "" {
		// ponytail: backend is inferred from interfaces, then pinned for the
		// life of the process. Reading the kube-flannel-cfg ConfigMap would be
		// authoritative but needs an RBAC verb the collector does not hold.
		if v.Link("flannel.1") != nil {
			f.mode = "vxlan"
		} else {
			f.mode = "host-gw"
		}
	}
	top := Topology{Mode: f.mode, NotApplicable: map[string]string{}}
	if f.mode == "host-gw" {
		for _, k := range Keys[4:10] {
			top.NotApplicable[k] = "flannel host-gw backend: no overlay tunnel"
		}
		top.NotApplicable[KeyNeighbourTable] = "flannel host-gw backend: no overlay tunnel"
	}
	return top
}

func (f *Flannel) Sample(_ context.Context, v *netlink.View, podCIDRs []string) map[string]sample.Field {
	out := map[string]sample.Field{}
	linkFields(v, KeyPodBridge, "cni0", out)
	linkFields(v, KeyOverlay, "flannel.1", out)
	out[KeyNeighbourTable] = neighbours(v, "flannel.1")
	common(v, podCIDRs, out)
	return out
}
