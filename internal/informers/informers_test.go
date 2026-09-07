package informers

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Perserverance-syn/Cluster-recorder/internal/diff"
	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
	"github.com/Perserverance-syn/Cluster-recorder/internal/store"
)

func pod(phase corev1.PodPhase, restarts int32, waiting string) *corev1.Pod {
	cs := corev1.ContainerStatus{Name: "app", RestartCount: restarts}
	if waiting != "" {
		cs.State.Waiting = &corev1.ContainerStateWaiting{Reason: waiting}
	}
	if restarts > 0 {
		cs.LastTerminationState.Terminated = &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "a", Name: "p"},
		Spec:       corev1.PodSpec{NodeName: "n1"},
		Status:     corev1.PodStatus{Phase: phase, ContainerStatuses: []corev1.ContainerStatus{cs}},
	}
}

func TestPodTransitionsOnlyRecordChanges(t *testing.T) {
	now := time.Now()
	if got := PodTransitions(pod("Running", 0, ""), pod("Running", 0, ""), now); len(got) != 0 {
		t.Fatalf("identical pods produced %+v", got)
	}
	got := PodTransitions(pod("Pending", 0, ""), pod("Running", 2, "CrashLoopBackOff"), now)
	kinds := map[string]store.PodTransition{}
	for _, tr := range got {
		kinds[tr.Kind] = tr
	}
	if len(got) != 3 {
		t.Fatalf("want phase+restart+waiting, got %+v", got)
	}
	if kinds[store.TransitionPhase].Old != "Pending" || kinds[store.TransitionPhase].New != "Running" {
		t.Errorf("phase: %+v", kinds[store.TransitionPhase])
	}
	if kinds[store.TransitionRestart].New != "2" || kinds[store.TransitionRestart].Detail != "container=app reason=OOMKilled exit=137" {
		t.Errorf("restart: %+v", kinds[store.TransitionRestart])
	}
	if kinds[store.TransitionWaiting].New != "CrashLoopBackOff" {
		t.Errorf("waiting: %+v", kinds[store.TransitionWaiting])
	}
	if s := PodStateOf(pod("Running", 2, "CrashLoopBackOff")); s.WaitingReason != "CrashLoopBackOff" || s.Restarts != 2 {
		t.Errorf("state: %+v", s)
	}
}

func node(ready corev1.ConditionStatus, bootID string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		Spec:       corev1.NodeSpec{PodCIDRs: []string{"10.244.1.0/24"}},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready}, {Type: corev1.NodeMemoryPressure, Status: "False"}},
			NodeInfo:   corev1.NodeSystemInfo{BootID: bootID, KubeletVersion: "v1.30.14"},
			Addresses:  []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "192.0.2.11"}},
		},
	}
}

func TestNodeSnapshotFieldStates(t *testing.T) {
	f := NodeSnapshot(node("True", "b1"), time.Now()).Fields
	if !f["node.ready"].Equal(sample.Present("True")) {
		t.Errorf("ready: %+v", f["node.ready"])
	}
	if f["node.network_unavailable"].State != sample.StateAbsent {
		t.Errorf("missing condition must be absent, got %+v", f["node.network_unavailable"])
	}
	if f["node.os_image"].State != sample.StateAbsent || !f["node.pod_cidrs"].Equal(sample.Present("10.244.1.0/24")) {
		t.Errorf("info fields: %+v", f)
	}
}

func TestEventRecordTimestamps(t *testing.T) {
	ts := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	ev := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{UID: "u", Namespace: "a", Name: "e"},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Namespace: "a", Name: "p"},
		EventTime:      metav1.MicroTime{Time: ts},
		Series:         &corev1.EventSeries{Count: 7, LastObservedTime: metav1.MicroTime{Time: ts.Add(time.Minute)}},
	}
	rec := EventRecord(ev, func(ns, name string) string { return "n1" })
	if rec.Node != "n1" || rec.Count != 7 || !rec.First.Equal(ts) || !rec.Last.Equal(ts.Add(time.Minute)) {
		t.Fatalf("record = %+v", rec)
	}
}

// End to end through real informers against a fake API server: a node
// condition flip must land in the timeline as an alert-free info change and
// a pod entering CrashLoopBackOff must land as a transition.
func TestCollectorAgainstFakeCluster(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "t.db"), slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	client := fake.NewSimpleClientset(node("True", "b1"), pod("Running", 0, ""))
	c, err := New(client, db, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor(t, c.Ready)

	n := node("False", "b2") // NotReady and rebooted
	if _, err := client.CoreV1().Nodes().Update(ctx, n, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.CoreV1().Pods("a").UpdateStatus(ctx, pod("Running", 3, "CrashLoopBackOff"), metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		tl, _ := db.Timeline(ctx, "n1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
		return len(tl) >= 4 // 2 changes + restart + waiting
	})
	changes, _ := db.Changes(ctx, "n1", time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	want := map[string]diff.Severity{"node.ready": diff.SeverityInfo, "node.boot_id": diff.SeverityInfo}
	for _, ch := range changes {
		if want[ch.Field] != ch.Severity {
			t.Errorf("unexpected change %+v", ch)
		}
		delete(want, ch.Field)
	}
	if len(want) != 0 {
		t.Errorf("missing changes: %v", want)
	}
	cl, _ := db.CrashLoopingPods(ctx)
	if len(cl) != 1 || cl[0] != "a/p" {
		t.Errorf("crashlooping = %v", cl)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestHintsDiscoverCIDRsAndDaemonSets(t *testing.T) {
	db, _ := store.Open(filepath.Join(t.TempDir(), "t.db"), slog.Default())
	defer db.Close()
	kcm := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "kube-controller-manager-cp", Labels: map[string]string{"component": "kube-controller-manager"}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "kcm", Command: []string{"kube-controller-manager", "--cluster-cidr=10.244.0.0/16", "--v=2"}}}}}
	ds := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Namespace: "kube-flannel", Name: "kube-flannel-ds"}}
	client := fake.NewSimpleClientset(node("True", "b1"), kcm, ds)
	c, err := New(client, db, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	waitFor(t, c.Ready)
	h, err := c.Hints("n1")
	if err != nil {
		t.Fatal(err)
	}
	if len(h.DaemonSets) != 1 || h.DaemonSets[0] != "kube-flannel/kube-flannel-ds" {
		t.Errorf("daemonsets = %v", h.DaemonSets)
	}
	if len(h.PodCIDRs) != 2 || h.PodCIDRs[0] != "10.244.1.0/24" || h.PodCIDRs[1] != "10.244.0.0/16" {
		t.Errorf("pod cidrs = %v", h.PodCIDRs)
	}
	if h, _ = c.Hints("ghost"); len(h.PodCIDRs) != 1 { // unknown node: cluster CIDR only, never a guess
		t.Errorf("unknown node cidrs = %v", h.PodCIDRs)
	}
}
