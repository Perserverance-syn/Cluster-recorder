package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Perserverance-syn/Cluster-recorder/internal/cni"
	"github.com/Perserverance-syn/Cluster-recorder/internal/netlink"
	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
)

func view() *netlink.View {
	return &netlink.View{
		Links:  []netlink.Link{{Name: "ens33", Type: "device", Up: true, Carrier: true}, {Name: "cni0", Type: "bridge", Up: true, MTU: 1450}, {Name: "flannel.1", Type: "vxlan", Up: true, MTU: 1450, VXLAN: &netlink.VXLAN{VNI: 1, Local: "192.0.2.12"}}},
		Addrs:  []netlink.Addr{{Link: "cni0", IP: "10.244.2.1", Prefix: 24, Family: 4}},
		Routes: []netlink.Route{{Dst: "0.0.0.0/0", Gateway: "192.0.2.1", Dev: "ens33", Family: 4}, {Dst: "10.244.1.0/24", Gateway: "10.244.1.0", Dev: "flannel.1", Family: 4}},
	}
}

func newAgent(t *testing.T, collectorURL string) *Agent {
	t.Helper()
	cfg := Config{CollectorURL: collectorURL, Node: "worker-2", Driver: "auto", CNIDir: t.TempDir(), ProcRoot: t.TempDir(), HostRoot: t.TempDir(),
		Interval: time.Second, BufferSize: 3, RedetectEvery: 10 * time.Minute}
	return New(cfg, netlink.Fixture{V: view()}, slog.Default())
}

func TestSnapshotUsesHintsAndDetectsFlannel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agent/hints" && r.URL.Query().Get("node") == "worker-2" {
			_ = json.NewEncoder(w).Encode(Hints{DaemonSets: []string{"kube-flannel/kube-flannel-ds"}, PodCIDRs: []string{"10.244.2.0/24", "10.244.0.0/16"}})
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	snap := newAgent(t, srv.URL).Snapshot(context.Background())
	if err := snap.Validate(); err != nil {
		t.Fatal(err)
	}
	if snap.Driver != "flannel" || snap.Node != "worker-2" || snap.Source != sample.SourceAgent {
		t.Fatalf("snapshot meta = %+v", snap)
	}
	if !snap.Fields[cni.KeyPodBridgeIPv4].Equal(sample.Present("10.244.2.1")) {
		t.Errorf("bridge ipv4 = %+v", snap.Fields[cni.KeyPodBridgeIPv4])
	}
	if !snap.Fields[cni.KeyPodCIDRRoutes].Equal(sample.Present([]string{"10.244.1.0/24 via 10.244.1.0 dev flannel.1"})) {
		t.Errorf("routes = %+v", snap.Fields[cni.KeyPodCIDRRoutes])
	}
	if snap.Fields["boot_time"].State != sample.StateUnknown { // fake proc root: read failure is unknown, never absent
		t.Errorf("boot_time = %+v", snap.Fields["boot_time"])
	}
}

func TestBufferSurvivesCollectorOutage(t *testing.T) {
	var down atomic.Bool
	var received atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(503)
			return
		}
		if r.Method == http.MethodPost {
			received.Add(1)
			w.WriteHeader(202)
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	a := newAgent(t, srv.URL)
	ctx := context.Background()

	down.Store(true)
	for i := 0; i < 5; i++ {
		a.tick(ctx)
	}
	if a.Buffered() != 3 {
		t.Fatalf("buffer must cap at 3 dropping oldest, got %d", a.Buffered())
	}
	down.Store(false)
	a.tick(ctx)
	if a.Buffered() != 0 || received.Load() != 3 { // cap applies to the whole backlog, newest sample included
		t.Fatalf("after recovery: buffered=%d received=%d, want 0 and 3", a.Buffered(), received.Load())
	}
}

func TestNoHintsNoOverrideMeansUnknownRoutes(t *testing.T) {
	a := newAgent(t, "http://127.0.0.1:1") // nothing listening
	snap := a.Snapshot(context.Background())
	if snap.Fields[cni.KeyPodCIDRRoutes].State != sample.StateUnknown {
		t.Fatalf("routes = %+v, want unknown", snap.Fields[cni.KeyPodCIDRRoutes])
	}
	a.Cfg.PodCIDR = "10.244.0.0/16"
	if snap = a.Snapshot(context.Background()); snap.Fields[cni.KeyPodCIDRRoutes].State != sample.StatePresent {
		t.Fatalf("override must apply: %+v", snap.Fields[cni.KeyPodCIDRRoutes])
	}
}
