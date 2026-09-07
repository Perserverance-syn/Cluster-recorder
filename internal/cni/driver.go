// Package cni maps a specific CNI's concrete interfaces onto generic field
// names. Adding CNI support means adding one file implementing Driver.
// Nothing outside this package ever sees a concrete interface name.
package cni

import (
	"context"
	"fmt"
	"net"
	"sort"

	"github.com/Perserverance-syn/Cluster-recorder/internal/netlink"
	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
)

// Generic field keys: the only names the diff engine, storage, API and UI see.
const (
	KeyPodBridge         = "pod_bridge"
	KeyPodBridgeIPv4     = "pod_bridge.ipv4"
	KeyPodBridgeMTU      = "pod_bridge.mtu"
	KeyPodBridgeState    = "pod_bridge.state"
	KeyOverlay           = "overlay_tunnel"
	KeyOverlayIPv4       = "overlay_tunnel.ipv4"
	KeyOverlayMTU        = "overlay_tunnel.mtu"
	KeyOverlayState      = "overlay_tunnel.state"
	KeyOverlayVNI        = "overlay_tunnel.vni"
	KeyOverlayLocal      = "overlay_tunnel.local_addr"
	KeyPodCIDRRoutes     = "pod_cidr_routes"
	KeyNeighbourTable    = "neighbour_table"
	KeyPrimaryNICState   = "primary_nic.state"
	KeyPrimaryNICCarrier = "primary_nic.carrier"
	KeyInterfaces        = "interfaces" // every non-loopback link; what the generic driver has to offer
)

var Keys = []string{
	KeyPodBridge, KeyPodBridgeIPv4, KeyPodBridgeMTU, KeyPodBridgeState,
	KeyOverlay, KeyOverlayIPv4, KeyOverlayMTU, KeyOverlayState, KeyOverlayVNI, KeyOverlayLocal,
	KeyPodCIDRRoutes, KeyNeighbourTable, KeyPrimaryNICState, KeyPrimaryNICCarrier, KeyInterfaces,
}

type CNIConfig struct {
	File        string
	PluginTypes []string
}

type DetectContext struct {
	CNIConfigs []CNIConfig // parsed /etc/cni/net.d/*.conf|.conflist
	DaemonSets []string    // "namespace/name" of DaemonSets in the cluster, from the collector
	Interfaces []string    // link names on this node
}

// Topology declares which generic fields this CNI has at all in its current
// mode. Keys in NotApplicable are reported not_applicable, never absent.
type Topology struct {
	NotApplicable map[string]string
	Mode          string // free-form, recorded for context: "vxlan", "host-gw", "ipip", "routing", "native", ...
}

// Driver is implemented once per CNI. Detect is scored 0..100; highest wins,
// ties break on registration order, generic always scores 1.
type Driver interface {
	Name() string
	Detect(DetectContext) (confidence int, reason string)
	// Topology may inspect the live view to infer the mode. Implementations
	// must be sticky: once an overlay has been seen, its later disappearance
	// is an outage, not a mode change.
	Topology(v *netlink.View) Topology
	// Sample reads CNI-scoped fields from the view. It never returns an error
	// for a single unreadable field and never panics on a missing interface.
	Sample(ctx context.Context, v *netlink.View, podCIDRs []string) map[string]sample.Field
}

// Result is what the agent posts, before host fields are merged in.
type Result struct {
	Driver     string
	Confidence int
	Reason     string
	Mode       string
	Fields     map[string]sample.Field
}

// Sample runs a driver against one netlink read and guarantees every generic
// key is present: not_applicable from the topology, unknown on read failure
// or when the driver forgot one, the driver's own value otherwise.
func Sample(ctx context.Context, d Driver, confidence int, reason string, nl netlink.Netlink, podCIDRs []string) Result {
	res := Result{Driver: d.Name(), Confidence: confidence, Reason: reason, Fields: map[string]sample.Field{}}
	v, err := nl.View(ctx)
	if err != nil {
		for _, k := range Keys {
			res.Fields[k] = sample.Unknown("netlink: " + err.Error())
		}
		return res
	}
	top := d.Topology(v)
	res.Mode = top.Mode
	got := d.Sample(ctx, v, podCIDRs)
	for _, k := range Keys {
		if why, na := top.NotApplicable[k]; na {
			res.Fields[k] = sample.NotApplicable(why)
			continue
		}
		f, ok := got[k]
		if !ok {
			f = sample.Unknown(fmt.Sprintf("driver %s did not report %s", d.Name(), k))
		}
		res.Fields[k] = f
	}
	return res
}

// --- helpers shared by drivers -------------------------------------------

// linkFields fills the four (or six, for VXLAN) fields describing one named
// link under a generic prefix. A missing link is absent, not unknown: the
// read succeeded and the thing is not there.
func linkFields(v *netlink.View, prefix, name string, out map[string]sample.Field) {
	l := v.Link(name)
	if l == nil {
		for _, suffix := range []string{"", ".ipv4", ".mtu", ".state"} {
			out[prefix+suffix] = sample.Absent()
		}
		if prefix == KeyOverlay {
			out[KeyOverlayVNI], out[KeyOverlayLocal] = sample.Absent(), sample.Absent()
		}
		return
	}
	out[prefix] = sample.Present(name)
	if ip := v.IPv4(name); ip != "" {
		out[prefix+".ipv4"] = sample.Present(ip)
	} else {
		out[prefix+".ipv4"] = sample.Absent()
	}
	out[prefix+".mtu"] = sample.Present(l.MTU)
	state := "down"
	if l.Up {
		state = "up"
	}
	out[prefix+".state"] = sample.Present(state)
	if prefix == KeyOverlay {
		if l.VXLAN != nil {
			out[KeyOverlayVNI] = sample.Present(l.VXLAN.VNI)
			if l.VXLAN.Local != "" {
				out[KeyOverlayLocal] = sample.Present(l.VXLAN.Local)
			} else {
				out[KeyOverlayLocal] = sample.Absent()
			}
		} else {
			out[KeyOverlayVNI], out[KeyOverlayLocal] = sample.NotApplicable("not a VXLAN device"), sample.NotApplicable("not a VXLAN device")
		}
	}
}

// podCIDRRoutes lists routes whose destination lies inside any pod CIDR, as
// "dst via gw dev X", sorted. Unknown when no CIDR was discovered: guessing
// one records the wrong routes forever.
func podCIDRRoutes(v *netlink.View, podCIDRs []string) sample.Field {
	if len(podCIDRs) == 0 {
		return sample.Unknown("pod CIDR not discovered; set POD_CIDR to override")
	}
	var nets []*net.IPNet
	for _, c := range podCIDRs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			nets = append(nets, n)
		}
	}
	routes := []string{}
	for _, r := range v.Routes {
		if r.IsDefault() {
			continue
		}
		ip, _, err := net.ParseCIDR(r.Dst)
		if err != nil {
			continue
		}
		for _, n := range nets {
			if n.Contains(ip) {
				s := r.Dst
				if r.Gateway != "" {
					s += " via " + r.Gateway
				}
				routes = append(routes, s+" dev "+r.Dev)
				break
			}
		}
	}
	sort.Strings(routes)
	return sample.Present(routes)
}

// neighbours lists the neighbour table of one device as "ip mac state", sorted.
func neighbours(v *netlink.View, dev string) sample.Field {
	if v.Link(dev) == nil {
		return sample.Absent()
	}
	out := []string{}
	for _, n := range v.Neighs {
		if n.Dev == dev && n.State != "NOARP" { // NOARP = IPv6 multicast noise
			out = append(out, n.IP+" "+n.MAC+" "+n.State)
		}
	}
	sort.Strings(out)
	return sample.Present(out)
}

// primaryNIC fills state and carrier of the device carrying the default route.
func primaryNIC(v *netlink.View, out map[string]sample.Field) {
	dev := v.DefaultDev()
	l := v.Link(dev)
	if dev == "" || l == nil {
		out[KeyPrimaryNICState], out[KeyPrimaryNICCarrier] = sample.Absent(), sample.Absent()
		return
	}
	state := "down"
	if l.Up {
		state = "up"
	}
	out[KeyPrimaryNICState] = sample.Present(dev + " " + state)
	out[KeyPrimaryNICCarrier] = sample.Present(l.Carrier)
}

// interfaces summarises every non-loopback, non-veth link as "name type state ipv4".
// veths come and go with every pod; pods are the API's job, not netlink's.
func interfaces(v *netlink.View) sample.Field {
	out := []string{}
	for _, l := range v.Links {
		if l.Name == "lo" || l.Type == "veth" {
			continue
		}
		state := "down"
		if l.Up {
			state = "up"
		}
		ip := v.IPv4(l.Name)
		if ip == "" {
			ip = "-"
		}
		out = append(out, l.Name+" "+l.Type+" "+state+" "+ip)
	}
	sort.Strings(out)
	return sample.Present(out)
}

// common fills the CNI-independent keys every driver reports identically.
func common(v *netlink.View, podCIDRs []string, out map[string]sample.Field) {
	out[KeyPodCIDRRoutes] = podCIDRRoutes(v, podCIDRs)
	primaryNIC(v, out)
	out[KeyInterfaces] = interfaces(v)
}

func hasAny(list []string, wanted ...string) bool {
	for _, l := range list {
		for _, w := range wanted {
			if l == w {
				return true
			}
		}
	}
	return false
}

func pluginTypes(ctx DetectContext) []string {
	var out []string
	for _, c := range ctx.CNIConfigs {
		out = append(out, c.PluginTypes...)
	}
	return out
}

func dsNames(ctx DetectContext) []string {
	var out []string
	for _, ds := range ctx.DaemonSets {
		if i := lastSlash(ds); i >= 0 {
			out = append(out, ds[i+1:])
		} else {
			out = append(out, ds)
		}
	}
	return out
}

func lastSlash(s string) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == '/' {
			return i
		}
	}
	return -1
}

// score combines detection signals: config is authoritative, a DaemonSet is
// strong, interfaces alone are a hint. Capped at 100.
func score(cfg, ds, iface bool, name string) (int, string) {
	n, why := 0, ""
	if cfg {
		n += 70
		why += name + " in CNI config; "
	}
	if ds {
		n += 40
		why += name + " DaemonSet present; "
	}
	if iface {
		n += 20
		why += name + " interfaces present; "
	}
	if n > 100 {
		n = 100
	}
	if why == "" {
		why = "no " + name + " signals"
	}
	return n, why
}
