package informers

import (
	"fmt"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
	"github.com/Perserverance-syn/Cluster-recorder/internal/store"
)

// EventRecord converts an API event into a stored one. Newer clusters leave
// LastTimestamp empty and use EventTime/Series; handle both or lose half.
func EventRecord(ev *corev1.Event, nodeOfPod func(ns, name string) string) store.Event {
	last := ev.LastTimestamp.Time
	if last.IsZero() && ev.Series != nil {
		last = ev.Series.LastObservedTime.Time
	}
	if last.IsZero() {
		last = ev.EventTime.Time
	}
	if last.IsZero() {
		last = ev.CreationTimestamp.Time
	}
	first := ev.FirstTimestamp.Time
	if first.IsZero() {
		first = ev.EventTime.Time
	}
	if first.IsZero() {
		first = last
	}
	count := ev.Count
	if count == 0 && ev.Series != nil {
		count = ev.Series.Count
	}
	if count == 0 {
		count = 1
	}
	node := ev.Source.Host
	switch ev.InvolvedObject.Kind {
	case "Node":
		node = ev.InvolvedObject.Name
	case "Pod":
		if n := nodeOfPod(ev.InvolvedObject.Namespace, ev.InvolvedObject.Name); n != "" {
			node = n
		}
	}
	return store.Event{
		UID: string(ev.UID), Namespace: ev.Namespace, Name: ev.Name, Type: ev.Type, Reason: ev.Reason, Message: ev.Message,
		Kind: ev.InvolvedObject.Kind, ObjNamespace: ev.InvolvedObject.Namespace, ObjName: ev.InvolvedObject.Name,
		Node: node, Count: count, First: first.UTC(), Last: last.UTC(),
	}
}

func containerStatuses(p *corev1.Pod) map[string]corev1.ContainerStatus {
	out := map[string]corev1.ContainerStatus{}
	for _, list := range [][]corev1.ContainerStatus{p.Status.InitContainerStatuses, p.Status.ContainerStatuses} {
		for _, cs := range list {
			out[cs.Name] = cs
		}
	}
	return out
}

func waitingReason(cs corev1.ContainerStatus) string {
	if cs.State.Waiting != nil {
		return cs.State.Waiting.Reason
	}
	return ""
}

// PodTransitions returns only what changed between two versions of a pod:
// phase changes, restart-count increments (with the terminated state that
// caused them), and container waiting-reason changes. Anything else is noise.
func PodTransitions(oldPod, newPod *corev1.Pod, now time.Time) []store.PodTransition {
	base := store.PodTransition{Time: now, Namespace: newPod.Namespace, Name: newPod.Name, Node: newPod.Spec.NodeName}
	var out []store.PodTransition
	if oldPod.Status.Phase != newPod.Status.Phase {
		t := base
		t.Kind, t.Old, t.New = store.TransitionPhase, string(oldPod.Status.Phase), string(newPod.Status.Phase)
		if newPod.Status.Reason != "" {
			t.Detail = newPod.Status.Reason
		}
		out = append(out, t)
	}
	oldCS := containerStatuses(oldPod)
	names := []string{}
	newCS := containerStatuses(newPod)
	for name := range newCS {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		n := newCS[name]
		o := oldCS[name]
		if n.RestartCount > o.RestartCount {
			t := base
			t.Kind, t.Old, t.New = store.TransitionRestart, fmt.Sprint(o.RestartCount), fmt.Sprint(n.RestartCount)
			if term := n.LastTerminationState.Terminated; term != nil {
				t.Detail = fmt.Sprintf("container=%s reason=%s exit=%d", name, term.Reason, term.ExitCode)
				if !term.FinishedAt.IsZero() {
					t.Time = term.FinishedAt.Time.UTC()
				}
			} else {
				t.Detail = "container=" + name
			}
			out = append(out, t)
		}
		if ow, nw := waitingReason(o), waitingReason(n); ow != nw {
			t := base
			t.Kind, t.Old, t.New, t.Detail = store.TransitionWaiting, ow, nw, "container="+name
			if n.State.Waiting != nil && n.State.Waiting.Message != "" {
				t.Detail += " " + n.State.Waiting.Message
			}
			out = append(out, t)
		}
	}
	return out
}

// PodStateOf summarises a pod for the current-state table. CrashLoopBackOff
// wins over other waiting reasons because it is what incidents key on.
func PodStateOf(p *corev1.Pod) store.PodState {
	s := store.PodState{Namespace: p.Namespace, Name: p.Name, Node: p.Spec.NodeName, Phase: string(p.Status.Phase)}
	for _, cs := range containerStatuses(p) {
		s.Restarts += cs.RestartCount
		if r := waitingReason(cs); r != "" && (s.WaitingReason == "" || r == "CrashLoopBackOff") {
			s.WaitingReason = r
		}
	}
	return s
}

// NodeSnapshot maps Node.status onto fields in the node.* namespace. These are
// collector-side and CNI-independent; agent fields live in a separate snapshot.
func NodeSnapshot(n *corev1.Node, now time.Time) *sample.Snapshot {
	f := map[string]sample.Field{}
	conds := map[corev1.NodeConditionType]string{}
	for _, c := range n.Status.Conditions {
		conds[c.Type] = string(c.Status)
	}
	for key, typ := range map[string]corev1.NodeConditionType{
		"node.ready":               corev1.NodeReady,
		"node.memory_pressure":     corev1.NodeMemoryPressure,
		"node.disk_pressure":       corev1.NodeDiskPressure,
		"node.pid_pressure":        corev1.NodePIDPressure,
		"node.network_unavailable": corev1.NodeNetworkUnavailable,
	} {
		if v, ok := conds[typ]; ok {
			f[key] = sample.Present(v)
		} else {
			f[key] = sample.Absent()
		}
	}
	f["node.unschedulable"] = sample.Present(n.Spec.Unschedulable)
	str := func(key, v string) {
		if v == "" {
			f[key] = sample.Absent()
		} else {
			f[key] = sample.Present(v)
		}
	}
	info := n.Status.NodeInfo
	str("node.boot_id", info.BootID) // changes on reboot: the tier-1 answer to "did this node reboot?"
	str("node.kernel_version", info.KernelVersion)
	str("node.os_image", info.OSImage)
	str("node.container_runtime", info.ContainerRuntimeVersion)
	str("node.kubelet_version", info.KubeletVersion)
	cidrs := n.Spec.PodCIDRs
	if len(cidrs) == 0 && n.Spec.PodCIDR != "" {
		cidrs = []string{n.Spec.PodCIDR}
	}
	str("node.pod_cidrs", strings.Join(cidrs, ","))
	ip := ""
	for _, a := range n.Status.Addresses {
		if a.Type == corev1.NodeInternalIP {
			ip = a.Address
			break
		}
	}
	str("node.internal_ip", ip)
	taints := []string{}
	for _, t := range n.Spec.Taints {
		taints = append(taints, t.Key+"="+t.Value+":"+string(t.Effect))
	}
	sort.Strings(taints)
	f["node.taints"] = sample.Present(taints)
	return &sample.Snapshot{Node: n.Name, Source: sample.SourceAPI, Time: now.UTC(), Fields: f}
}
