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
	cs    *CSCloud
	queue map[serviceKey]queueEntry
}

type queueEntry struct {
	service *v1.Service
	nodes   []*v1.Node
	start   time.Time
}

func (q *serviceNodeQueue) Upsert(service *v1.Service, nodes []*v1.Node) {
	q.Lock()
	defer q.Unlock()

	if q.queue == nil {
		q.queue = map[serviceKey]queueEntry{}
	}

	key := serviceKey{
		namespace: service.Namespace,
		name:      service.Name,
	}

	if entry, ok := q.queue[key]; ok {
		entry.nodes = nodes
		return
	}

	q.queue[key] = queueEntry{
		service: service.DeepCopy(),
		nodes:   nodes,
		start:   time.Now(),
	}
}

func (q *serviceNodeQueue) Pop() (queueEntry, bool, error) {
	q.Lock()
	defer q.Unlock()

	for k, v := range q.queue {
		delete(q.queue, k)
		return v, true, nil
	}

	return queueEntry{}, false, nil
}

func (cs *CSCloud) startLBUpdateQueue(ctx context.Context) *serviceNodeQueue {
	workers := cs.config.Global.UpdateLBWorkers
	if workers == 0 {
		workers = defaultUpdateLBWorkers
	}
	var queue serviceNodeQueue
	for i := 0; i < workers; i++ {
		go func() {
			waitCh := time.After(0)
			for {
				select {
				case <-ctx.Done():
					return
				case <-waitCh:
				}

				item, ok, err := queue.Pop()
				if err != nil {
					klog.Errorf("unable to pop item from queue: %v", err)
					continue
				}

				if !ok {
					waitCh = time.After(time.Second)
					continue
				}

				err = cs.processQueueEntry(item)
				processedTotal.WithLabelValues(item.service.Namespace, item.service.Name).Inc()
				processedDuration.WithLabelValues(item.service.Namespace, item.service.Name).Set(time.Since(item.start).Seconds())
				if err != nil {
					failuresTotal.WithLabelValues(item.service.Namespace, item.service.Name).Inc()
					cs.recorder.Eventf(item.service, corev1.EventTypeWarning, eventReasonUpdateFailed, "Error updating load balancer with new hosts %v: %v", nodeNames(item.nodes), err)
				}
			}
		}()
	}
	return &queue
}

func (cs *CSCloud) processQueueEntry(entry queueEntry) error {
	cs.svcLock.Lock(entry.service)
	defer cs.svcLock.Unlock(entry.service)

	entry.nodes = cs.filterNodesMatchingLabels(entry.nodes, entry.service)

	hostIDs, networkIDs, projectID, err := cs.extractIDs(entry.nodes)
	if err != nil {
		return err
	}

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
