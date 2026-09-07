package cni

import (
	"context"

	"github.com/Perserverance-syn/Cluster-recorder/internal/netlink"
	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
)

// Calico routes to veths directly: there is never a pod bridge, so pod_bridge
// is always not_applicable. A driver reporting absent here would alarm
// forever on a healthy cluster. overlay_tunnel is vxlan.calico (VXLAN mode),
// tunl0 (IPIP mode) or not_applicable (BGP/routing mode).
type Calico struct {
	mode string // sticky
}

func (*Calico) Name() string { return "calico" }

func (*Calico) Detect(ctx DetectContext) (int, string) {
	return score(hasAny(pluginTypes(ctx), "calico"),
		hasAny(dsNames(ctx), "calico-node"),
		hasAny(ctx.Interfaces, "vxlan.calico", "tunl0") && !hasAny(ctx.Interfaces, "cni0"), "calico")
}

func (c *Calico) tunnel() string {
	switch c.mode {
	case "vxlan":
		return "vxlan.calico"
	case "ipip":
		return "tunl0"
	}
	return ""
}

func (c *Calico) Topology(v *netlink.View) Topology {
	if c.mode == "" {
		switch {
		case v.Link("vxlan.calico") != nil:
			c.mode = "vxlan"
		case v.Link("tunl0") != nil && v.IPv4("tunl0") != "":
			c.mode = "ipip" // tunl0 exists on any node with the ipip module loaded; an address means it is in use
		default:
			c.mode = "routing"
		}
	}
	top := Topology{Mode: c.mode, NotApplicable: map[string]string{}}
	for _, k := range Keys[:4] {
		top.NotApplicable[k] = "calico routes to veths directly: no pod bridge"
	}
	if c.mode == "routing" {
		for _, k := range Keys[4:10] {
			top.NotApplicable[k] = "calico routing (BGP) mode: no overlay tunnel"
		}
		top.NotApplicable[KeyNeighbourTable] = "calico routing (BGP) mode: no overlay tunnel"
	}
	return top
}

func (c *Calico) Sample(_ context.Context, v *netlink.View, podCIDRs []string) map[string]sample.Field {
	out := map[string]sample.Field{}
	if t := c.tunnel(); t != "" {
		linkFields(v, KeyOverlay, t, out)
		out[KeyNeighbourTable] = neighbours(v, t)
	}
	common(v, podCIDRs, out)
	return out
}
