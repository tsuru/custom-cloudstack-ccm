package cloudstack

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	corev1 "k8s.io/api/core/v1"
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
	service *corev1.Service
	start   time.Time
}

type queueEntryWithNodeRecent struct {
	queueEntry
	topRevision uint64
}

func newServiceNodeQueue(cs *CSCloud) *serviceNodeQueue {
	return &serviceNodeQueue{
		cs:    cs,
		queue: map[serviceKey]queueEntry{},
	}
}

func (q *serviceNodeQueue) upsert(service *corev1.Service) error {
	q.Lock()
	defer q.Unlock()

	key := serviceKey{
		namespace: service.Namespace,
		name:      service.Name,
	}

	if _, ok := q.queue[key]; ok {
		return nil
	}

	// Simply validate that there are nodes available, we'll fetch them
	// directly from the registry when running the task.
	_, err := q.cs.nodeRegistry.nodesForService(service)
	if err != nil {
		return err
	}

	if q.queue == nil {
		q.queue = map[serviceKey]queueEntry{}
	}

	q.queue[key] = queueEntry{
		service: service,
		start:   time.Now(),
	}

	return nil
}

func (q *serviceNodeQueue) pop() (queueEntry, bool, error) {
	q.Lock()
	defer q.Unlock()

	var extendEntries []queueEntryWithNodeRecent

	for svcKey, entry := range q.queue {
		topRevision := uint64(0)
		svcNodes := q.cs.nodeRegistry.nodesContainingService(svcKey)
		for _, node := range svcNodes {
			if node.revision > topRevision {
				topRevision = node.revision
			}
		}
		extendEntries = append(extendEntries, queueEntryWithNodeRecent{
			queueEntry:  entry,
			topRevision: topRevision,
		})
	}

	if len(extendEntries) == 0 {
		return queueEntry{}, false, nil
	}

	sort.Slice(extendEntries, func(i, j int) bool {
		if extendEntries[i].topRevision == extendEntries[j].topRevision {
			return extendEntries[i].start.Before(extendEntries[j].start)
		}
		return extendEntries[i].topRevision > extendEntries[j].topRevision
	})

	entry := extendEntries[0].queueEntry
	delete(q.queue, serviceKey{namespace: entry.service.Namespace, name: entry.service.Name})
	return extendEntries[0].queueEntry, true, nil
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

				err = q.processQueueEntry(item)
				processedTotal.WithLabelValues(item.service.Namespace, item.service.Name).Inc()
				processedDuration.WithLabelValues(item.service.Namespace, item.service.Name).Set(time.Since(item.start).Seconds())
				if err != nil {
					failuresTotal.WithLabelValues(item.service.Namespace, item.service.Name).Inc()
					q.cs.recorder.Eventf(item.service, corev1.EventTypeWarning, eventReasonUpdateFailed, "Error updating load balancer with new hosts: %v", err)
					q.upsert(item.service)
				}
			}
		}()
	}
}

func (q *serviceNodeQueue) processQueueEntry(entry queueEntry) error {
	q.cs.svcLock.Lock(entry.service)
	defer q.cs.svcLock.Unlock(entry.service)

	nodes, err := q.cs.nodeRegistry.nodesForService(entry.service)
	if err != nil {
		return err
	}

	hostIDs, networkIDs, projectID := idsForNodes(nodes)

	// Get the load balancer details and existing rules.
	lb, err := q.cs.getLoadBalancer(entry.service, projectID, networkIDs)
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
