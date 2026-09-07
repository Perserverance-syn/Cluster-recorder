//go:build linux

package netlink

import (
	"context"
	"fmt"

	vnl "github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// Host reads the network namespace the process runs in. With hostNetwork
// that is the node. Read-only: no capabilities are needed.
type Host struct{}

func (Host) View(ctx context.Context) (*View, error) {
	links, err := vnl.LinkList()
	if err != nil {
		return nil, fmt.Errorf("link list: %w", err)
	}
	byIndex := map[int]string{}
	v := &View{}
	for _, l := range links {
		a := l.Attrs()
		byIndex[a.Index] = a.Name
		link := Link{Name: a.Name, Index: a.Index, Type: l.Type(), Up: a.Flags&unix.IFF_UP != 0,
			Carrier: a.OperState == vnl.OperUp || a.OperState == vnl.OperUnknown, MTU: a.MTU}
		if vx, ok := l.(*vnl.Vxlan); ok {
			link.VXLAN = &VXLAN{VNI: vx.VxlanId, Port: vx.Port}
			if vx.SrcAddr != nil {
				link.VXLAN.Local = vx.SrcAddr.String()
			}
		}
		v.Links = append(v.Links, link)
	}
	for i := range v.Links {
		if idx := links[i].Attrs().MasterIndex; idx != 0 {
			v.Links[i].Master = byIndex[idx]
		}
	}
	for _, fam := range []int{unix.AF_INET, unix.AF_INET6} {
		family := 4
		if fam == unix.AF_INET6 {
			family = 6
		}
		addrs, err := vnl.AddrList(nil, fam)
		if err != nil {
			return nil, fmt.Errorf("addr list: %w", err)
		}
		for _, a := range addrs {
			ones, _ := a.IPNet.Mask.Size()
			v.Addrs = append(v.Addrs, Addr{Link: byIndex[a.LinkIndex], IP: a.IP.String(), Prefix: ones, Family: family})
		}
		routes, err := vnl.RouteList(nil, fam)
		if err != nil {
			return nil, fmt.Errorf("route list: %w", err)
		}
		for _, r := range routes {
			route := Route{Dev: byIndex[r.LinkIndex], Family: family}
			if r.Dst != nil {
				route.Dst = r.Dst.String()
			}
			if r.Gw != nil {
				route.Gateway = r.Gw.String()
			}
			if r.Src != nil {
				route.Src = r.Src.String()
			}
			v.Routes = append(v.Routes, route)
		}
		neighs, err := vnl.NeighList(0, fam)
		if err != nil {
			return nil, fmt.Errorf("neigh list: %w", err)
		}
		for _, n := range neighs {
			if n.IP == nil {
				continue
			}
			v.Neighs = append(v.Neighs, Neigh{Dev: byIndex[n.LinkIndex], IP: n.IP.String(), MAC: n.HardwareAddr.String(), State: neighState(n.State)})
		}
	}
	return v, nil
}

func neighState(s int) string {
	names := map[int]string{
		vnl.NUD_INCOMPLETE: "INCOMPLETE", vnl.NUD_REACHABLE: "REACHABLE", vnl.NUD_STALE: "STALE",
		vnl.NUD_DELAY: "DELAY", vnl.NUD_PROBE: "PROBE", vnl.NUD_FAILED: "FAILED",
		vnl.NUD_NOARP: "NOARP", vnl.NUD_PERMANENT: "PERMANENT", vnl.NUD_NONE: "NONE",
	}
	if n, ok := names[s]; ok {
		return n
	}
	return fmt.Sprintf("0x%x", s)
}
