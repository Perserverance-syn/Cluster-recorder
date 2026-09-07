//go:build !linux

package netlink

import (
	"context"
	"errors"
)

// Host is unavailable off Linux; the agent only ships for Linux nodes.
type Host struct{}

func (Host) View(context.Context) (*View, error) {
	return nil, errors.New("netlink: host reads are Linux-only")
}
