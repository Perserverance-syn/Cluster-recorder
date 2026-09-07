//go:build !linux

package host

import "errors"

func diskUsage(string) (int, int, error) { return 0, 0, errors.New("statfs is Linux-only") }
