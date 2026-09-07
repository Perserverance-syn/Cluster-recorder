// Package netlink is the only package that imports a netlink library. It
// exposes one read-only View of the host's links, addresses, routes and
// neighbours, and a fixture-backed implementation so drivers are testable
// without root or a node.
package netlink

import (
	"context"
	"encoding/json"
	"os"
	"sort"
)

type Link struct {
	Name    string `json:"name"`
	Index   int    `json:"index"`
	Type    string `json:"type"` // bridge, vxlan, veth, device, ipip, geneve, ...
	Up      bool   `json:"up"`   // administratively up (IFF_UP)
	Carrier bool   `json:"carrier"`
	MTU     int    `json:"mtu"`
	Master  string `json:"master,omitempty"`
	VXLAN   *VXLAN `json:"vxlan,omitempty"`
}

type VXLAN struct {
	VNI   int    `json:"vni"`
	Local string `json:"local"`
	Port  int    `json:"port"`
}

type Addr struct {
	Link   string `json:"link"`
	IP     string `json:"ip"`
	Prefix int    `json:"prefix"`
	Family int    `json:"family"` // 4 or 6
}

type Route struct {
	Dst     string `json:"dst"` // "" means default
	Gateway string `json:"gateway,omitempty"`
	Dev     string `json:"dev"`
	Src     string `json:"src,omitempty"`
	Family  int    `json:"family"`
}

type Neigh struct {
	Dev   string `json:"dev"`
	IP    string `json:"ip"`
	MAC   string `json:"mac"`
	State string `json:"state"` // PERMANENT, REACHABLE, STALE, FAILED, ...
}

// View is one consistent read of the host network namespace.
type View struct {
	Links  []Link  `json:"links"`
	Addrs  []Addr  `json:"addrs"`
	Routes []Route `json:"routes"`
	Neighs []Neigh `json:"neighs"`
}

// Netlink reads the host. The real implementation is Linux-only; tests use Fixture.
type Netlink interface {
	View(ctx context.Context) (*View, error)
}

// Fixture serves a View decoded from JSON. Capture one from a node with
// `agent --dump`.
type Fixture struct{ V *View }

func (f Fixture) View(context.Context) (*View, error) { return f.V, nil }

// Failing simulates a read failure (permission denied, timeout).
type Failing struct{ Err error }

func (f Failing) View(context.Context) (*View, error) { return nil, f.Err }

func LoadFixture(path string) (*View, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v View
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

// Link returns the named link, or nil.
func (v *View) Link(name string) *Link {
	for i := range v.Links {
		if v.Links[i].Name == name {
			return &v.Links[i]
		}
	}
	return nil
}

// IPv4 returns the first IPv4 address on a link, or "".
func (v *View) IPv4(link string) string {
	for _, a := range v.Addrs {
		if a.Link == link && a.Family == 4 {
			return a.IP
		}
	}
	return ""
}

// LinkNames lists all link names, sorted.
func (v *View) LinkNames() []string {
	out := make([]string, 0, len(v.Links))
	for _, l := range v.Links {
		out = append(out, l.Name)
	}
	sort.Strings(out)
	return out
}

// IsDefault reports whether the route is a default route. The library reports
// the destination as 0.0.0.0/0 or ::/0 on recent kernels and empty on older ones.
func (r Route) IsDefault() bool { return r.Dst == "" || r.Dst == "0.0.0.0/0" || r.Dst == "::/0" }

// DefaultDev is the device carrying the IPv4 default route, or "".
func (v *View) DefaultDev() string {
	for _, r := range v.Routes {
		if r.IsDefault() && r.Family == 4 {
			return r.Dev
		}
	}
	return ""
}
