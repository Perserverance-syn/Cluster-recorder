package cni

import (
	"context"

	"github.com/Perserverance-syn/Cluster-recorder/internal/netlink"
	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
)

// Cilium: pod_bridge = cilium_host (the node-side veth of the host pair),
// overlay_tunnel = cilium_vxlan or cilium_geneve, not_applicable in native
// routing mode.
type Cilium struct {
	mode string // sticky
}

func (*Cilium) Name() string { return "cilium" }

func (*Cilium) Detect(ctx DetectContext) (int, string) {
	return score(hasAny(pluginTypes(ctx), "cilium-cni"),
		hasAny(dsNames(ctx), "cilium"),
		hasAny(ctx.Interfaces, "cilium_host"), "cilium")
}

func (c *Cilium) tunnel() string {
	switch c.mode {
	case "vxlan":
		return "cilium_vxlan"
	case "geneve":
		return "cilium_geneve"
	}
	return ""
}

func (c *Cilium) Topology(v *netlink.View) Topology {
	if c.mode == "" {
		switch {
		case v.Link("cilium_vxlan") != nil:
			c.mode = "vxlan"
		case v.Link("cilium_geneve") != nil:
			c.mode = "geneve"
		default:
			c.mode = "native"
		}
	}
	top := Topology{Mode: c.mode, NotApplicable: map[string]string{}}
	if c.mode == "native" {
		for _, k := range Keys[4:10] {
			top.NotApplicable[k] = "cilium native routing mode: no overlay tunnel"
		}
		top.NotApplicable[KeyNeighbourTable] = "cilium native routing mode: no overlay tunnel"
	}
	return top
}

func (c *Cilium) Sample(_ context.Context, v *netlink.View, podCIDRs []string) map[string]sample.Field {
	out := map[string]sample.Field{}
	linkFields(v, KeyPodBridge, "cilium_host", out)
	if t := c.tunnel(); t != "" {
		linkFields(v, KeyOverlay, t, out)
		out[KeyNeighbourTable] = neighbours(v, t)
	}
	common(v, podCIDRs, out)
	return out
}
