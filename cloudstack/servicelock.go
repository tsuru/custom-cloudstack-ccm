package cloudstack

import (
	"sync"

	v1 "k8s.io/api/core/v1"
)

type serviceKey struct {
	namespace string
	name      string
}

type refCounter struct {
	counter int
	lock    sync.Mutex
}

type serviceLock struct {
	mu    sync.Mutex
	inUse map[serviceKey]*refCounter
}

func (l *serviceLock) Lock(svc *v1.Service) {
	l.mu.Lock()
	m := l.getLocker(serviceKey{namespace: svc.Namespace, name: svc.Name})
	m.counter++
	l.mu.Unlock()
	m.lock.Lock()
}

func (l *serviceLock) Unlock(svc *v1.Service) {
	key := serviceKey{namespace: svc.Namespace, name: svc.Name}
	l.mu.Lock()
	defer l.mu.Unlock()
	m := l.getLocker(key)
	m.lock.Unlock()
	m.counter--
	if m.counter <= 0 {
		delete(l.inUse, key)
	}
}

func (l *serviceLock) getLocker(key serviceKey) *refCounter {
	if l.inUse == nil {
		l.inUse = make(map[serviceKey]*refCounter)
	}
	if l.inUse[key] == nil {
		l.inUse[key] = &refCounter{counter: 0}
	}
	return l.inUse[key]
}
