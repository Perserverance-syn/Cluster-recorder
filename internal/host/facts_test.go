package host

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
)

func fakeProc(t *testing.T) string {
	t.Helper()
	proc := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(proc, rel)
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("stat", "cpu 1 2 3\nbtime 1700000000\nprocesses 5\n")
	write("meminfo", "MemTotal:       1000000 kB\nMemFree:          50000 kB\nMemAvailable:     60000 kB\n")
	write("42/comm", "kubelet\n")
	// starttime (field 22) = 12000 ticks = 120s after boot; comm with a space to prove the parser skips it.
	write("42/stat", "42 (kubelet) S 1 42 42 0 -1 4194560 100 0 0 0 50 20 0 0 20 0 10 0 12000 1000 100 18446744073709551615 0 0 0 0 0 0 0 0 0 0 0 0 17 0 0 0 0 0 0")
	write("99/comm", "sshd\n")
	return proc
}

func TestFields(t *testing.T) {
	f := Fields(fakeProc(t), t.TempDir())
	if !f[KeyBootTime].Equal(sample.Present(1700000000)) {
		t.Errorf("boot_time = %+v", f[KeyBootTime])
	}
	if !f[KeyKubeletActive].Equal(sample.Present(true)) || !f[KeyKubeletStarted].Equal(sample.Present(1700000120)) {
		t.Errorf("kubelet = %+v %+v", f[KeyKubeletActive], f[KeyKubeletStarted])
	}
	if !f[KeyRuntimeActive].Equal(sample.Present(false)) || f[KeyRuntimeStarted].State != sample.StateAbsent {
		t.Errorf("containerd missing must be present(false)/absent, got %+v %+v", f[KeyRuntimeActive], f[KeyRuntimeStarted])
	}
	if !f[KeyMemoryPressure].Equal(sample.Present(true)) { // 6% available < 10%
		t.Errorf("memory_pressure = %+v", f[KeyMemoryPressure])
	}
	for _, k := range Keys {
		if _, ok := f[k]; !ok {
			t.Errorf("missing key %s", k)
		}
	}
}

func TestUnreadableProcIsUnknown(t *testing.T) {
	f := Fields(filepath.Join(t.TempDir(), "nope"), "/")
	if f[KeyBootTime].State != sample.StateUnknown || f[KeyKubeletActive].State != sample.StateUnknown {
		t.Fatalf("read failure must be unknown, got %+v", f)
	}
}
