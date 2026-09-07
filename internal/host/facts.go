// Package host samples the CNI-independent node fields: boot time, kubelet and
// container runtime processes, memory, disk and inode pressure. Everything
// comes from /proc and statfs; no D-Bus, no shelling out.
package host

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
)

const (
	KeyBootTime         = "boot_time"
	KeyKubeletActive    = "kubelet.active"
	KeyKubeletStarted   = "kubelet.started_at" // changes on restart; a count needs systemd, which we do not talk to
	KeyRuntimeActive    = "container_runtime.active"
	KeyRuntimeStarted   = "container_runtime.started_at"
	KeyMemoryPressure   = "memory_pressure"
	KeyDiskPressure     = "disk_pressure"
	KeyInodeUsage       = "inode_usage"
	clockTicksPerSecond = 100 // Linux USER_HZ; the kernel exposes 100 to userspace on every mainstream arch
)

// ponytail: fixed thresholds; make them config if anyone tunes kubelet's eviction thresholds.
const (
	memoryPressureBelowAvailPct = 10
	diskPressureAbovePct        = 90
)

var Keys = []string{KeyBootTime, KeyKubeletActive, KeyKubeletStarted, KeyRuntimeActive, KeyRuntimeStarted, KeyMemoryPressure, KeyDiskPressure, KeyInodeUsage}

// Fields samples the node. proc is the host's /proc (with hostPID the
// container's own /proc); root is a read-only mount of the host filesystem.
func Fields(proc, root string) map[string]sample.Field {
	f := map[string]sample.Field{}
	btime, err := bootTime(proc)
	if err != nil {
		f[KeyBootTime] = sample.Unknown(err.Error())
	} else {
		f[KeyBootTime] = sample.Present(btime)
	}
	for name, keys := range map[string][2]string{
		"kubelet":    {KeyKubeletActive, KeyKubeletStarted},
		"containerd": {KeyRuntimeActive, KeyRuntimeStarted},
	} {
		start, found, err := processStart(proc, name, btime)
		switch {
		case err != nil:
			f[keys[0]], f[keys[1]] = sample.Unknown(err.Error()), sample.Unknown(err.Error())
		case !found:
			f[keys[0]], f[keys[1]] = sample.Present(false), sample.Absent()
		default:
			f[keys[0]], f[keys[1]] = sample.Present(true), sample.Present(start)
		}
	}
	if pct, err := memAvailablePct(proc); err != nil {
		f[KeyMemoryPressure] = sample.Unknown(err.Error())
	} else {
		f[KeyMemoryPressure] = sample.Present(pct < memoryPressureBelowAvailPct)
	}
	if used, inodes, err := diskUsage(root); err != nil {
		f[KeyDiskPressure], f[KeyInodeUsage] = sample.Unknown(err.Error()), sample.Unknown(err.Error())
	} else {
		f[KeyDiskPressure], f[KeyInodeUsage] = sample.Present(used > diskPressureAbovePct), sample.Present(inodes)
	}
	return f
}

func bootTime(proc string) (int64, error) {
	fh, err := os.Open(filepath.Join(proc, "stat"))
	if err != nil {
		return 0, err
	}
	defer fh.Close()
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			return strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		}
	}
	return 0, fmt.Errorf("no btime in %s/stat", proc)
}

// processStart finds a process by exact comm name and returns its start
// time as Unix seconds. The lowest PID wins when several match.
func processStart(proc, comm string, btime int64) (int64, bool, error) {
	dirs, err := os.ReadDir(proc)
	if err != nil {
		return 0, false, err
	}
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join(proc, d.Name(), "comm"))
		if err != nil || strings.TrimSpace(string(b)) != comm {
			continue
		}
		stat, err := os.ReadFile(filepath.Join(proc, d.Name(), "stat"))
		if err != nil {
			return 0, false, err
		}
		// Field 22 (starttime) comes after the parenthesised comm, which may contain spaces.
		s := string(stat)
		rest := s[strings.LastIndex(s, ")")+1:]
		fields := strings.Fields(rest) // fields[0] is state (field 3)
		if len(fields) < 20 {
			return 0, false, fmt.Errorf("pid %d: short stat", pid)
		}
		ticks, err := strconv.ParseInt(fields[19], 10, 64)
		if err != nil {
			return 0, false, err
		}
		return btime + ticks/clockTicksPerSecond, true, nil
	}
	return 0, false, nil
}

func memAvailablePct(proc string) (int, error) {
	fh, err := os.Open(filepath.Join(proc, "meminfo"))
	if err != nil {
		return 0, err
	}
	defer fh.Close()
	var total, avail int64
	sc := bufio.NewScanner(fh)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		n, _ := strconv.ParseInt(fields[1], 10, 64)
		switch fields[0] {
		case "MemTotal:":
			total = n
		case "MemAvailable:":
			avail = n
		}
	}
	if total == 0 {
		return 0, fmt.Errorf("no MemTotal in %s/meminfo", proc)
	}
	return int(avail * 100 / total), nil
}
