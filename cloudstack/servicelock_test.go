package cloudstack

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func Test_serviceLock(t *testing.T) {
	svcs := []*v1.Service{
		{ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "n1"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "svc1", Namespace: "n2"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "svc2", Namespace: "n1"}},
	}
	lock := &serviceLock{}
	allWg := sync.WaitGroup{}
	nGoroutines := 50
	for c := 0; c < nGoroutines; c++ {
		c := c
		allWg.Add(1)
		go func() {
			defer allWg.Done()
			wg := sync.WaitGroup{}
			for i := range svcs {
				i := i
				wg.Add(1)
				go func() {
					defer wg.Done()
					lock.Lock(svcs[i])
					if svcs[i].Labels == nil {
						svcs[i].Labels = map[string]string{}
					}
					svcs[i].Labels[fmt.Sprintf("l-%d", c)] = "true"
					lock.Unlock(svcs[i])
				}()
			}
			wg.Wait()
		}()
	}

	done := make(chan struct{})
	go func() {
		allWg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Error("timeout waiting for all locks to be released")
	}

	assert.Len(t, lock.inUse, 0)
	for c := 0; c < nGoroutines; c++ {
		for _, svc := range svcs {
			assert.Equal(t, "true", svc.Labels[fmt.Sprintf("l-%d", c)])
		}
	}
}
