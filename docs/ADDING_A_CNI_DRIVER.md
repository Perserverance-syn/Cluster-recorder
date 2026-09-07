# Adding a CNI driver

One file in `internal/cni/`. Nothing outside that package changes. The Calico driver was written second
precisely to prove that; if your driver needs a change elsewhere, that is a bug in the abstraction, file it.

## The `Driver` interface, method by method

```go
type Driver interface {
    Name() string
    Detect(DetectContext) (confidence int, reason string)
    Topology(v *netlink.View) Topology
    Sample(ctx context.Context, v *netlink.View, podCIDRs []string) map[string]sample.Field
}
```

**`Name()`** — stable identifier, recorded with every snapshot and shown in `/api/v1/nodes`. Lowercase, no spaces.

**`Detect(ctx)`** — score 0..100 that this driver matches the node. You get the parsed `/etc/cni/net.d` plugin
types, every DaemonSet in the cluster as `namespace/name` (supplied by the collector, the agent has no API
access), and the node's interface names. Use the shared `score(cfg, ds, iface, name)` helper: config is worth 70,
a DaemonSet 40, interfaces 20, capped at 100. Highest wins; ties break on registration order; generic scores 1.
Return a reason string: it is stored and it is the first thing anyone reads when detection was wrong.

**`Topology(v)`** — declare which generic fields *cannot exist* on this CNI in its current mode, with a reason
each. Anything listed here is reported `not_applicable` and never alarms. This is the method that stops a
routed CNI from being read as a broken overlay. It may inspect the live view to infer the mode, and it **must be
sticky**: keep the mode in a struct field and never downgrade from "has an overlay" to "has none" during the
process lifetime. An overlay that disappears is an outage, not a mode change.

**`Sample(ctx, v, podCIDRs)`** — map concrete interfaces onto generic keys. Use the helpers: `linkFields`
(name, ipv4, mtu, state, and vni/local_addr for the overlay), `neighbours`, and `common` (pod CIDR routes,
primary NIC, interface list). Never return an error for one unreadable thing; never panic on a missing
interface. A missing interface is `absent`; a read failure is handled above you as `unknown`.

Every key in `cni.Keys` is guaranteed to reach the collector: the framework fills `not_applicable` from your
topology and `unknown` for anything you forgot. Forgetting one shows up in the API as
`driver X did not report Y`, so you will notice.

## Worked example: a driver for Weave

Weave has a bridge (`weave`), a VXLAN datapath device (`datapath`, or `vethwe-datapath`), and a DaemonSet
`weave-net` in `kube-system`. Plugin type is `weave-net`.

```go
package cni

import (
	"context"

	"github.com/Perserverance-syn/Cluster-recorder/internal/netlink"
	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
)

type Weave struct{ mode string }

func (*Weave) Name() string { return "weave" }

func (*Weave) Detect(ctx DetectContext) (int, string) {
	return score(hasAny(pluginTypes(ctx), "weave-net"),
		hasAny(dsNames(ctx), "weave-net"),
		hasAny(ctx.Interfaces, "weave"), "weave")
}

func (w *Weave) Topology(v *netlink.View) Topology {
	if w.mode == "" {
		if v.Link("datapath") != nil {
			w.mode = "fastdp"
		} else {
			w.mode = "sleeve"
		}
	}
	top := Topology{Mode: w.mode, NotApplicable: map[string]string{}}
	if w.mode == "sleeve" { // userspace UDP: no kernel overlay device to inspect
		for _, k := range Keys[4:10] {
			top.NotApplicable[k] = "weave sleeve mode: no kernel overlay device"
		}
		top.NotApplicable[KeyNeighbourTable] = "weave sleeve mode: no kernel overlay device"
	}
	return top
}

func (w *Weave) Sample(_ context.Context, v *netlink.View, podCIDRs []string) map[string]sample.Field {
	out := map[string]sample.Field{}
	linkFields(v, KeyPodBridge, "weave", out)
	if w.mode == "fastdp" {
		linkFields(v, KeyOverlay, "datapath", out)
		out[KeyNeighbourTable] = neighbours(v, "datapath")
	}
	common(v, podCIDRs, out)
	return out
}
```

Register it in `detect.go` before `Generic{}`:

```go
func Registry() []Driver { return []Driver{&Flannel{}, &Calico{}, &Cilium{}, &Weave{}, Generic{}} }
```

## Capturing a fixture from a real node

```bash
GOOS=linux CGO_ENABLED=0 go build -o /tmp/nldump ./hack/nldump
scp /tmp/nldump worker:/tmp/ && ssh worker 'chmod +x /tmp/nldump && sudo /tmp/nldump /' > internal/cni/testdata/weave-worker.json
```

The file holds the full netlink view (links, addresses, routes, neighbours) plus the host fields. Commit it.
It contains node IPs and MACs; nothing secret.

## Testing without a cluster

Load the fixture, derive the failure cases by mutation, assert on generic keys only:

```go
func TestWeaveHealthy(t *testing.T) {
	f := run(t, &Weave{}, load(t, "weave-worker"))
	expect(t, f, KeyPodBridge, sample.Present("weave"))
}

func TestWeaveBridgeLostIPv4(t *testing.T) {
	f := run(t, &Weave{}, withoutAddr(load(t, "weave-worker"), "weave"))
	expect(t, f, KeyPodBridgeIPv4, sample.Absent())
}
```

Every driver needs: healthy, overlay-missing (after the healthy read, so mode is pinned), bridge-missing-IPv4,
and its not-applicable mode. `run` already asserts every key is present and valid. Netlink failure is tested
once for the framework, not per driver.

## Where `not_applicable` applies, and why guessing wrong is worse than `unknown`

`not_applicable` means "by design, never". Use it only when the CNI's documented mode says so. If you cannot
tell which mode is active, do not guess `not_applicable`: an outage on a field you declared non-existent is
invisible forever. Leave the field to be sampled; worst case it reads `absent` on a healthy node and someone
pins the driver or fixes the mode inference. That is a loud false alarm, which gets fixed. A silent miss does not.

Likewise never turn a read failure into `absent`. The framework already gives you `unknown` for free.