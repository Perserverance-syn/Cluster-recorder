package cni

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Registry order is the tie-break order. Generic is last and scores 1.
func Registry() []Driver { return []Driver{&Flannel{}, &Calico{}, &Cilium{}, Generic{}} }

// Choice is the recorded outcome of detection.
type Choice struct {
	Driver     Driver
	Confidence int
	Reason     string
}

// Choose runs detection, or honours a forced driver name ("auto" or "" means detect).
func Choose(force string, ctx DetectContext) (Choice, error) {
	if force != "" && force != "auto" {
		for _, d := range Registry() {
			if d.Name() == force {
				return Choice{Driver: d, Confidence: 100, Reason: "forced by CNI_DRIVER=" + force}, nil
			}
		}
		return Choice{}, fmt.Errorf("CNI_DRIVER=%q: no such driver (have %s)", force, strings.Join(Names(), ", "))
	}
	best := Choice{}
	for _, d := range Registry() {
		c, why := d.Detect(ctx)
		if best.Driver == nil || c > best.Confidence {
			best = Choice{Driver: d, Confidence: c, Reason: why}
		}
	}
	return best, nil
}

func Names() []string {
	var out []string
	for _, d := range Registry() {
		out = append(out, d.Name())
	}
	return out
}

// ParseCNIDir reads /etc/cni/net.d and returns the plugin types per file.
// A missing or unreadable directory is not an error: detection still has
// DaemonSets and interfaces to go on.
func ParseCNIDir(dir string) []CNIConfig {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []CNIConfig
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".conf") && !strings.HasSuffix(name, ".conflist") && !strings.HasSuffix(name, ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var doc struct {
			Type    string `json:"type"`
			Plugins []struct {
				Type string `json:"type"`
			} `json:"plugins"`
		}
		if json.Unmarshal(b, &doc) != nil {
			continue
		}
		cfg := CNIConfig{File: name}
		if doc.Type != "" {
			cfg.PluginTypes = append(cfg.PluginTypes, doc.Type)
		}
		for _, p := range doc.Plugins {
			cfg.PluginTypes = append(cfg.PluginTypes, p.Type)
		}
		out = append(out, cfg)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].File < out[j].File })
	return out
}
