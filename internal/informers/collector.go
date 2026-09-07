// Package informers watches Events, Pods and Nodes through shared informers and
// writes what matters through to the store: every event, pod transitions
// (not every update), and node snapshots with their diffs.
package informers

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8sinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	appslisters "k8s.io/client-go/listers/apps/v1"
	listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"

	"github.com/Perserverance-syn/Cluster-recorder/internal/diff"
	"github.com/Perserverance-syn/Cluster-recorder/internal/sample"
	"github.com/Perserverance-syn/Cluster-recorder/internal/store"
)

const resync = 5 * time.Minute

type Collector struct {
	db      *store.DB
	log     *slog.Logger
	factory k8sinformers.SharedInformerFactory
	pods    listers.PodLister
	nodes   listers.NodeLister
	// daemonsets feed CNI detection hints to agents, which have no API access.
	daemonsets appslisters.DaemonSetLister
	synced     atomic.Bool
	now        func() time.Time
}

func New(client kubernetes.Interface, db *store.DB, log *slog.Logger) (*Collector, error) {
	f := k8sinformers.NewSharedInformerFactory(client, resync)
	c := &Collector{db: db, log: log, factory: f, pods: f.Core().V1().Pods().Lister(), nodes: f.Core().V1().Nodes().Lister(),
		daemonsets: f.Apps().V1().DaemonSets().Lister(), now: time.Now}

	_, err := f.Core().V1().Events().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { c.onEvent(o) },
		UpdateFunc: func(_, o any) { c.onEvent(o) },
		// Deletes are the API server expiring events. Ignoring them is the whole point.
	})
	if err != nil {
		return nil, err
	}
	_, err = f.Core().V1().Pods().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { c.onPod(nil, o) },
		UpdateFunc: c.onPod,
		DeleteFunc: c.onPodDelete,
	})
	if err != nil {
		return nil, err
	}
	_, err = f.Core().V1().Nodes().Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { c.onNode(o) },
		UpdateFunc: func(_, o any) { c.onNode(o) },
	})
	if err != nil {
		return nil, err
	}
	return c, nil
}

// Run starts the informers, waits for the initial sync, and blocks until ctx ends.
func (c *Collector) Run(ctx context.Context) error {
	c.factory.Start(ctx.Done())
	for typ, ok := range c.factory.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return fmt.Errorf("informer cache sync failed for %v", typ)
		}
	}
	c.synced.Store(true)
	c.log.Info("informers synced")
	<-ctx.Done()
	return nil
}

// Ready reports whether the initial cache sync has completed.
func (c *Collector) Ready() bool { return c.synced.Load() }

func (c *Collector) nodeOfPod(ns, name string) string {
	p, err := c.pods.Pods(ns).Get(name)
	if err != nil {
		return ""
	}
	return p.Spec.NodeName
}

func (c *Collector) onEvent(o any) {
	ev, ok := o.(*corev1.Event)
	if !ok {
		return
	}
	if err := c.db.PutEvent(context.Background(), EventRecord(ev, c.nodeOfPod)); err != nil {
		c.log.Error("store event", "err", err)
	}
}

func (c *Collector) onPod(oldObj, newObj any) {
	newPod, ok := newObj.(*corev1.Pod)
	if !ok {
		return
	}
	ctx := context.Background()
	now := c.now()
	if oldPod, ok := oldObj.(*corev1.Pod); ok {
		if ts := PodTransitions(oldPod, newPod, now); len(ts) > 0 {
			if err := c.db.PutPodTransitions(ctx, ts); err != nil {
				c.log.Error("store pod transitions", "err", err)
			}
		}
	}
	if err := c.db.UpsertPod(ctx, PodStateOf(newPod), now); err != nil {
		c.log.Error("store pod state", "err", err)
	}
}

func (c *Collector) onPodDelete(o any) {
	if d, ok := o.(cache.DeletedFinalStateUnknown); ok {
		o = d.Obj
	}
	p, ok := o.(*corev1.Pod)
	if !ok {
		return
	}
	ctx := context.Background()
	t := store.PodTransition{Time: c.now(), Namespace: p.Namespace, Name: p.Name, Node: p.Spec.NodeName,
		Kind: store.TransitionPhase, Old: string(p.Status.Phase), New: "Deleted"}
	if err := c.db.PutPodTransitions(ctx, []store.PodTransition{t}); err != nil {
		c.log.Error("store pod delete", "err", err)
	}
	if err := c.db.ForgetPod(ctx, p.Namespace+"/"+p.Name); err != nil {
		c.log.Error("forget pod", "err", err)
	}
}

func (c *Collector) onNode(o any) {
	n, ok := o.(*corev1.Node)
	if !ok {
		return
	}
	if err := c.RecordNode(context.Background(), n); err != nil {
		c.log.Error("record node", "node", n.Name, "err", err)
	}
}

// RecordNode snapshots a node from its API object, diffs it against the
// previous api snapshot, and persists both. Unchanged resyncs write nothing.
func (c *Collector) RecordNode(ctx context.Context, n *corev1.Node) error {
	snap := NodeSnapshot(n, c.now())
	prev, err := c.db.LatestSnapshot(ctx, n.Name, sample.SourceAPI)
	if err != nil {
		return err
	}
	if prev != nil && sample.FieldsEqual(prev.Fields, snap.Fields) {
		return nil
	}
	if err := c.db.PutSnapshot(ctx, snap); err != nil {
		return err
	}
	changes := diff.Snapshots(prev, snap)
	for _, ch := range changes {
		c.log.Info("node change", "node", ch.Node, "field", ch.Field, "severity", ch.Severity, "old", ch.Old, "new", ch.New)
	}
	return c.db.PutChanges(ctx, changes)
}
