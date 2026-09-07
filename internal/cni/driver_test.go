package cni

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/Perserverance-syn/Cluster-recorder/internal/netlink"
	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
)

// Fixtures are captured from real nodes with hack/nldump. Variants are
// derived in-test so each failure mode is one mutation away from healthy.
func load(t *testing.T, name string) *netlink.View {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ View netlink.View }
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	return &doc.View
}

func without(v *netlink.View, link string) *netlink.View {
	out := &netlink.View{}
	for _, l := range v.Links {
		if l.Name != link {
			out.Links = append(out.Links, l)
		}
	}
	for _, a := range v.Addrs {
		if a.Link != link {
			out.Addrs = append(out.Addrs, a)
		}
	}
	for _, r := range v.Routes {
		if r.Dev != link {
			out.Routes = append(out.Routes, r)
		}
	}
	for _, n := range v.Neighs {
		if n.Dev != link {
			out.Neighs = append(out.Neighs, n)
		}
	}
	return out
}

func withoutAddr(v *netlink.View, link string) *netlink.View {
	out := *v
	out.Addrs = nil
	for _, a := range v.Addrs {
		if a.Link != link {
			out.Addrs = append(out.Addrs, a)
		}
	}
	return &out
}

var podCIDRs = []string{"10.244.0.0/16"}

func run(t *testing.T, d Driver, v *netlink.View) map[string]sample.Field {
	t.Helper()
	res := Sample(context.Background(), d, 100, "test", netlink.Fixture{V: v}, podCIDRs)
	for _, k := range Keys {
		if _, ok := res.Fields[k]; !ok {
			t.Fatalf("%s: key %s missing", d.Name(), k)
		}
		if err := res.Fields[k].Validate(); err != nil {
			t.Fatalf("%s: %s: %v", d.Name(), k, err)
		}
	}
	return res.Fields
}

func expect(t *testing.T, f map[string]sample.Field, key string, want sample.Field) {
	t.Helper()
	if got := f[key]; !got.Equal(want) {
		t.Errorf("%s = %+v, want %+v", key, got, want)
	}
}

func TestFlannelHealthy(t *testing.T) {
	f := run(t, &Flannel{}, load(t, "flannel-vxlan-worker"))
	expect(t, f, KeyPodBridge, sample.Present("cni0"))
	expect(t, f, KeyPodBridgeIPv4, sample.Present("10.244.2.1"))
	expect(t, f, KeyPodBridgeMTU, sample.Present(1450))
	expect(t, f, KeyOverlay, sample.Present("flannel.1"))
	expect(t, f, KeyOverlayVNI, sample.Present(1))
	expect(t, f, KeyOverlayLocal, sample.Present("192.0.2.12"))
	expect(t, f, KeyPodCIDRRoutes, sample.Present([]string{
		"10.244.0.0/24 via 10.244.0.0 dev flannel.1", "10.244.1.0/24 via 10.244.1.0 dev flannel.1", "10.244.2.0/24 dev cni0"}))
	expect(t, f, KeyNeighbourTable, sample.Present([]string{"10.244.0.0 92:c7:b0:cf:3c:49 PERMANENT", "10.244.1.0 72:89:02:c2:38:47 PERMANENT"}))
	expect(t, f, KeyPrimaryNICState, sample.Present("ens33 up"))
	expect(t, f, KeyPrimaryNICCarrier, sample.Present(true))
}

// The reference outage: cni0 loses its IPv4, peer routes vanish.
func TestFlannelBridgeLostIPv4(t *testing.T) {
	f := run(t, &Flannel{}, withoutAddr(load(t, "flannel-vxlan-worker"), "cni0"))
	expect(t, f, KeyPodBridge, sample.Present("cni0"))
	expect(t, f, KeyPodBridgeIPv4, sample.Absent())
}

func TestFlannelOverlayVanishedIsAbsentNotNA(t *testing.T) {
	d := &Flannel{}
	healthy := load(t, "flannel-vxlan-worker")
	run(t, d, healthy) // pins vxlan mode
	f := run(t, d, without(healthy, "flannel.1"))
	expect(t, f, KeyOverlay, sample.Absent())
	expect(t, f, KeyOverlayVNI, sample.Absent())
	expect(t, f, KeyNeighbourTable, sample.Absent())
	if len(f[KeyPodCIDRRoutes].Value.([]string)) != 1 {
		t.Errorf("peer routes should be gone with the tunnel: %+v", f[KeyPodCIDRRoutes])
	}
}

func TestFlannelHostGWIsNotApplicable(t *testing.T) {
	f := run(t, &Flannel{}, without(load(t, "flannel-vxlan-worker"), "flannel.1")) // never saw a tunnel
	expect(t, f, KeyOverlay, sample.NotApplicable("flannel host-gw backend: no overlay tunnel"))
	expect(t, f, KeyNeighbourTable, sample.NotApplicable("flannel host-gw backend: no overlay tunnel"))
	expect(t, f, KeyPodBridge, sample.Present("cni0"))
}

func TestNetlinkFailureIsUnknownEverywhere(t *testing.T) {
	res := Sample(context.Background(), &Flannel{}, 100, "t", netlink.Failing{Err: errors.New("permission denied")}, podCIDRs)
	for _, k := range Keys {
		if res.Fields[k].State != sample.StateUnknown {
			t.Fatalf("%s = %+v, want unknown", k, res.Fields[k])
		}
	}
}

func TestNoPodCIDRIsUnknownNotGuessed(t *testing.T) {
	res := Sample(context.Background(), &Flannel{}, 100, "t", netlink.Fixture{V: load(t, "flannel-vxlan-worker")}, nil)
	if res.Fields[KeyPodCIDRRoutes].State != sample.StateUnknown {
		t.Fatalf("routes without a CIDR must be unknown, got %+v", res.Fields[KeyPodCIDRRoutes])
	}
}

func calicoView(mode string) *netlink.View {
	v := &netlink.View{
		Links:  []netlink.Link{{Name: "ens33", Type: "device", Up: true, Carrier: true, MTU: 1500}, {Name: "cali1234", Type: "veth", Up: true}},
		Addrs:  []netlink.Addr{{Link: "ens33", IP: "192.0.2.10", Prefix: 24, Family: 4}},
		Routes: []netlink.Route{{Dst: "", Gateway: "192.0.2.1", Dev: "ens33", Family: 4}, {Dst: "10.244.1.0/26", Gateway: "192.0.2.11", Dev: "ens33", Family: 4}},
	}
	switch mode {
	case "vxlan":
		v.Links = append(v.Links, netlink.Link{Name: "vxlan.calico", Type: "vxlan", Up: true, MTU: 1450, VXLAN: &netlink.VXLAN{VNI: 4096, Local: "192.0.2.10"}})
		v.Addrs = append(v.Addrs, netlink.Addr{Link: "vxlan.calico", IP: "10.244.0.1", Prefix: 32, Family: 4})
	case "ipip":
		v.Links = append(v.Links, netlink.Link{Name: "tunl0", Type: "ipip", Up: true, MTU: 1480})
		v.Addrs = append(v.Addrs, netlink.Addr{Link: "tunl0", IP: "10.244.0.1", Prefix: 32, Family: 4})
	case "routing-with-idle-tunl0":
		v.Links = append(v.Links, netlink.Link{Name: "tunl0", Type: "ipip", Up: false})
	}
	return v
}

func TestCalicoNeverReportsBridgeAbsent(t *testing.T) {
	for _, mode := range []string{"vxlan", "ipip", "routing", "routing-with-idle-tunl0"} {
		f := run(t, &Calico{}, calicoView(mode))
		if f[KeyPodBridge].State != sample.StateNotApplicable || f[KeyPodBridgeIPv4].State != sample.StateNotApplicable {
			t.Errorf("%s: pod_bridge must be not_applicable, got %+v", mode, f[KeyPodBridge])
		}
	}
	f := run(t, &Calico{}, calicoView("vxlan"))
	expect(t, f, KeyOverlay, sample.Present("vxlan.calico"))
	expect(t, f, KeyOverlayVNI, sample.Present(4096))
	f = run(t, &Calico{}, calicoView("ipip"))
	expect(t, f, KeyOverlay, sample.Present("tunl0"))
	expect(t, f, KeyOverlayVNI, sample.NotApplicable("not a VXLAN device"))
	f = run(t, &Calico{}, calicoView("routing"))
	expect(t, f, KeyOverlay, sample.NotApplicable("calico routing (BGP) mode: no overlay tunnel"))
	f = run(t, &Calico{}, calicoView("routing-with-idle-tunl0"))
	expect(t, f, KeyOverlay, sample.NotApplicable("calico routing (BGP) mode: no overlay tunnel"))
	expect(t, f, KeyPodCIDRRoutes, sample.Present([]string{"10.244.1.0/26 via 192.0.2.11 dev ens33"}))
}

func TestCiliumNativeRouting(t *testing.T) {
	v := &netlink.View{
		Links:  []netlink.Link{{Name: "eth0", Type: "device", Up: true, Carrier: true}, {Name: "cilium_host", Type: "veth", Up: true, MTU: 1500}},
		Addrs:  []netlink.Addr{{Link: "cilium_host", IP: "10.0.0.1", Prefix: 32, Family: 4}},
		Routes: []netlink.Route{{Dst: "", Gateway: "192.0.2.1", Dev: "eth0", Family: 4}},
	}
	f := run(t, &Cilium{}, v)
	expect(t, f, KeyPodBridge, sample.Present("cilium_host"))
	expect(t, f, KeyOverlay, sample.NotApplicable("cilium native routing mode: no overlay tunnel"))
}

// Universal by default: an unrecognised CNI still yields a full, valid snapshot.
func TestGenericOnUnrecognisedCNI(t *testing.T) {
	v := load(t, "flannel-vxlan-worker")
	ctx := DetectContext{CNIConfigs: []CNIConfig{{File: "10-weird.conflist", PluginTypes: []string{"weirdnet"}}}, Interfaces: v.LinkNames()}
	ctx.Interfaces = removeAll(ctx.Interfaces, "cni0", "flannel.1")
	choice, err := Choose("auto", ctx)
	if err != nil || choice.Driver.Name() != "generic" {
		t.Fatalf("choice = %+v err %v", choice, err)
	}
	f := run(t, choice.Driver, without(without(v, "cni0"), "flannel.1"))
	expect(t, f, KeyPodBridge, sample.NotApplicable("unrecognised CNI"))
	if f[KeyInterfaces].State != sample.StatePresent || len(f[KeyInterfaces].Value.([]string)) == 0 {
		t.Fatalf("generic must list interfaces: %+v", f[KeyInterfaces])
	}
	expect(t, f, KeyPrimaryNICState, sample.Present("ens33 up"))
}

func removeAll(list []string, drop ...string) []string {
	var out []string
	for _, l := range list {
		if !hasAny(drop, l) {
			out = append(out, l)
		}
	}
	return out
}

func TestDetection(t *testing.T) {
	cases := []struct {
		name string
		ctx  DetectContext
		want string
	}{
		{"flannel by config", DetectContext{CNIConfigs: []CNIConfig{{PluginTypes: []string{"flannel", "portmap"}}}}, "flannel"},
		{"flannel by daemonset+iface", DetectContext{DaemonSets: []string{"kube-flannel/kube-flannel-ds"}, Interfaces: []string{"cni0", "flannel.1"}}, "flannel"},
		{"calico by config", DetectContext{CNIConfigs: []CNIConfig{{PluginTypes: []string{"calico", "bandwidth"}}}}, "calico"},
		{"calico by daemonset", DetectContext{DaemonSets: []string{"calico-system/calico-node"}}, "calico"},
		{"cilium by config", DetectContext{CNIConfigs: []CNIConfig{{PluginTypes: []string{"cilium-cni"}}}}, "cilium"},
		{"nothing", DetectContext{}, "generic"},
		{"config beats stray interface", DetectContext{CNIConfigs: []CNIConfig{{PluginTypes: []string{"calico"}}}, Interfaces: []string{"cni0"}}, "calico"},
	}
	for _, c := range cases {
		got, err := Choose("auto", c.ctx)
		if err != nil || got.Driver.Name() != c.want {
			t.Errorf("%s: got %s (%d, %s) err %v", c.name, got.Driver.Name(), got.Confidence, got.Reason, err)
		}
	}
	if _, err := Choose("weave", DetectContext{}); err == nil {
		t.Error("unknown forced driver must error")
	}
	forced, _ := Choose("generic", DetectContext{CNIConfigs: []CNIConfig{{PluginTypes: []string{"flannel"}}}})
	if forced.Driver.Name() != "generic" {
		t.Error("forced driver must win")
	}
}

func TestParseCNIDir(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(dir+"/10-flannel.conflist", []byte(`{"name":"cbr0","cniVersion":"0.3.1","plugins":[{"type":"flannel","delegate":{}},{"type":"portmap"}]}`), 0o644)
	_ = os.WriteFile(dir+"/99-loopback.conf", []byte(`{"type":"loopback"}`), 0o644)
	_ = os.WriteFile(dir+"/README", []byte("x"), 0o644)
	got := ParseCNIDir(dir)
	if len(got) != 2 || got[0].File != "10-flannel.conflist" || got[0].PluginTypes[0] != "flannel" || got[1].PluginTypes[0] != "loopback" {
		t.Fatalf("got %+v", got)
	}
	if ParseCNIDir(dir+"/missing") != nil {
		t.Fatal("missing dir must be nil, not an error")
	}
}
