package cloudstack

import (
	"context"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog"
)

const (
	defaultUpdateLBWorkers = 5

	promNamespace = "csccm"
	promSubsystem = "update_lb_queue"

	eventReasonUpdateFailed = "UpdateLoadBalancerFailed"
)

var (
	processedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: promNamespace,
		Subsystem: promSubsystem,
		Name:      "processed_total",
		Help:      "The number of processed queue items",
	}, []string{"namespace", "service"})

	failuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Namespace: promNamespace,
		Subsystem: promSubsystem,
		Name:      "failures_total",
		Help:      "The number of failed queue items",
	}, []string{"namespace", "service"})

	processedDuration = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: promNamespace,
		Subsystem: promSubsystem,
		Name:      "item_duration_seconds",
		Help:      "The duration of the last processed queue item",
	}, []string{"namespace", "service"})
)

type serviceNodeQueue struct {
	sync.Mutex
	cs     *CSCloud
	queue  map[serviceKey]queueEntry
	doneWG sync.WaitGroup
	stopCh chan struct{}
}

type queueEntry struct {
	service *v1.Service
	nodes   []nodeInfo
	start   time.Time
}

func newServiceNodeQueue(cs *CSCloud) *serviceNodeQueue {
	return &serviceNodeQueue{
		cs:    cs,
		queue: map[serviceKey]queueEntry{},
	}
}

func (q *serviceNodeQueue) upsert(service *v1.Service) error {
	q.Lock()
	defer q.Unlock()

	nodes, err := q.cs.nodeRegistry.nodesForService(service)
	if err != nil {
		return err
	}

	if q.queue == nil {
		q.queue = map[serviceKey]queueEntry{}
	}

	key := serviceKey{
		namespace: service.Namespace,
		name:      service.Name,
	}

	q.queue[key] = queueEntry{
		service: service,
		nodes:   nodes,
		start:   time.Now(),
	}

	return nil
}

func (q *serviceNodeQueue) pop() (queueEntry, bool, error) {
	q.Lock()
	defer q.Unlock()

	topRevision := uint64(0)
	var topSvcKey, emptySvcKey serviceKey

	for svcKey := range q.queue {
		svcNodes := q.cs.nodeRegistry.nodesContainingService(svcKey)
		if topSvcKey == emptySvcKey {
			topSvcKey = svcKey
		}
		for _, node := range svcNodes {
			if node.revision > topRevision {
				topRevision = node.revision
				topSvcKey = svcKey
			}
		}
	}

	if topSvcKey == emptySvcKey {
		return queueEntry{}, false, nil
	}

	entry := q.queue[topSvcKey]
	delete(q.queue, topSvcKey)
	return entry, true, nil
}

func (q *serviceNodeQueue) stopWait() {
	close(q.stopCh)
	q.doneWG.Wait()
}

func (q *serviceNodeQueue) start(ctx context.Context) {
	workers := q.cs.config.Global.UpdateLBWorkers
	if workers == 0 {
		workers = defaultUpdateLBWorkers
	}
	q.stopCh = make(chan struct{})
	for i := 0; i < workers; i++ {
		q.doneWG.Add(1)
		go func() {
			defer q.doneWG.Done()
			waitCh := time.After(0)
			exitWhenDone := false
			for {
				select {
				case <-ctx.Done():
					return
				case <-q.stopCh:
					exitWhenDone = true
				case <-waitCh:
				}

				item, ok, err := q.pop()
				if err != nil {
					klog.Errorf("unable to pop item from queue: %v", err)
					continue
				}

				if !ok {
					if exitWhenDone {
						return
					}
					waitCh = time.After(time.Second)
					continue
				}

				err = q.cs.processQueueEntry(item)
				processedTotal.WithLabelValues(item.service.Namespace, item.service.Name).Inc()
				processedDuration.WithLabelValues(item.service.Namespace, item.service.Name).Set(time.Since(item.start).Seconds())
				if err != nil {
					failuresTotal.WithLabelValues(item.service.Namespace, item.service.Name).Inc()
					q.cs.recorder.Eventf(item.service, corev1.EventTypeWarning, eventReasonUpdateFailed, "Error updating load balancer with new hosts %v: %v", item.nodes, err)
					q.upsert(item.service)
				}
			}
		}()
	}
}

func (cs *CSCloud) processQueueEntry(entry queueEntry) error {
	cs.svcLock.Lock(entry.service)
	defer cs.svcLock.Unlock(entry.service)

	hostIDs, networkIDs, projectID := idsForNodes(entry.nodes)

	// Get the load balancer details and existing rules.
	lb, err := cs.getLoadBalancer(entry.service, projectID, networkIDs)
	if err != nil {
		return err
	}

	if lb.rule == nil {
		return nil
	}

	err = shouldManageLB(lb)
	if err != nil {
		klog.V(3).Infof("Skipping UpdateLoadBalancer for service %s/%s: %v", entry.service.Namespace, entry.service.Name, err)
		return nil
	}

	return lb.syncNodes(hostIDs, networkIDs)
}
