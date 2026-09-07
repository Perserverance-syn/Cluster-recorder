package cni

import (
	"context"

	"github.com/Perserverance-syn/Cluster-recorder/internal/netlink"
	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
)

// Generic is the floor: it wins only when nothing else matches and records
// everything real on the node under neutral names. Degraded, never broken,
// never noisy: bridge and overlay are not_applicable, not absent.
type Generic struct{}

func (Generic) Name() string { return "generic" }

func (Generic) Detect(DetectContext) (int, string) { return 1, "fallback: no recognised CNI" }

func (Generic) Topology(*netlink.View) Topology {
	na := map[string]string{}
	for _, k := range Keys[:10] { // pod_bridge.* and overlay_tunnel.*
		na[k] = "unrecognised CNI"
	}
	na[KeyNeighbourTable] = "unrecognised CNI: no overlay device to inspect"
	return Topology{NotApplicable: na, Mode: "unknown"}
}

func (Generic) Sample(_ context.Context, v *netlink.View, podCIDRs []string) map[string]sample.Field {
	out := map[string]sample.Field{}
	common(v, podCIDRs, out)
	return out
}
