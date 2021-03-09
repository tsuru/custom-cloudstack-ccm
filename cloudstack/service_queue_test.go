package cloudstack

import (
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	cloudstackFake "github.com/tsuru/custom-cloudstack-ccm/cloudstack/fake"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func preparePopTest(t *testing.T) (*CSCloud, func()) {
	srv := cloudstackFake.NewCloudstackServer()
	cs := newTestCSCloud(t, &CSConfig{
		Global: globalConfig{
			EnvironmentLabel:   "environment-label",
			ProjectIDLabel:     "my/project-label",
			NodeFilterLabel:    "pool-label",
			ServiceFilterLabel: "pool-label",
		},
		Environment: map[string]*environmentConfig{
			"env1": {
				APIURL:          srv.URL,
				APIKey:          "a",
				SecretKey:       "b",
				LBEnvironmentID: "1",
				LBDomain:        "test.com",
				ProjectID:       "def-proj1",
			},
		},
	}, nil)

	nodes := []*corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "n2"},
		},
	}
	err := cs.nodeRegistry.updateNodes(nodes)
	require.NoError(t, err)

	nodes = []*corev1.Node{
		{
			ObjectMeta: metav1.ObjectMeta{Name: "n1"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "n2"},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: "n3"},
		},
	}
	err = cs.nodeRegistry.updateNodes(nodes)
	require.NoError(t, err)

	endpoints := []corev1.Endpoints{
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s1"},
			Subsets: []corev1.EndpointSubset{
				{
					Addresses: []corev1.EndpointAddress{
						{
							NodeName: strPtr("n1"),
						},
						{
							NodeName: strPtr("n2"),
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s2"},
			Subsets: []corev1.EndpointSubset{
				{
					Addresses: []corev1.EndpointAddress{
						{
							NodeName: strPtr("n1"),
						},
						{
							NodeName: strPtr("n3"),
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s3"},
			Subsets: []corev1.EndpointSubset{
				{
					Addresses: []corev1.EndpointAddress{
						{
							NodeName: strPtr("n2"),
						},
					},
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s4"},
			Subsets: []corev1.EndpointSubset{
				{
					Addresses: []corev1.EndpointAddress{
						{
							NodeName: strPtr("n3"),
						},
					},
				},
			},
		},
	}
	for _, ep := range endpoints {
		cs.nodeRegistry.updateEndpointsNodes(&ep)
	}

	return cs, srv.Close
}

func Test_serviceNodeQueue_upsert_pop(t *testing.T) {
	cs, cleanup := preparePopTest(t)
	defer cleanup()

	err := cs.updateLBQueue.push(queueEntry{service: &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s3"},
	}, start: time.Now()})
	require.NoError(t, err)
	err = cs.updateLBQueue.push(queueEntry{service: &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s2"},
	}, start: time.Now()})
	require.NoError(t, err)
	err = cs.updateLBQueue.push(queueEntry{service: &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s1"},
	}, start: time.Now()})
	require.NoError(t, err)

	entry, ok, err := cs.updateLBQueue.pop()
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "s2", entry.service.Name)

	entry, ok, err = cs.updateLBQueue.pop()
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "s3", entry.service.Name)

	entry, ok, err = cs.updateLBQueue.pop()
	require.NoError(t, err)
	assert.True(t, ok)
	assert.Equal(t, "s1", entry.service.Name)

	_, ok, err = cs.updateLBQueue.pop()
	require.NoError(t, err)
	assert.False(t, ok)
}

func Test_serviceNodeQueue_upsertWithBackoff_pop(t *testing.T) {
	minRetryDelay = 500 * time.Millisecond
	cs, cleanup := preparePopTest(t)
	defer cleanup()

	err := cs.updateLBQueue.push(queueEntry{service: &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s3"},
	}, start: time.Now()})
	require.NoError(t, err)
	backoff, err := cs.updateLBQueue.pushWithBackoff(queueEntry{service: &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s2"},
	}, start: time.Now()})
	require.NoError(t, err)
	assert.Equal(t, minRetryDelay, backoff)
	err = cs.updateLBQueue.push(queueEntry{service: &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s1"},
	}, start: time.Now()})
	require.NoError(t, err)
	err = cs.updateLBQueue.push(queueEntry{service: &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s4"},
	}, start: time.Now()})
	require.NoError(t, err)

	entry, ok, err := cs.updateLBQueue.pop()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "s4", entry.service.Name)

	entry, ok, err = cs.updateLBQueue.pop()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "s3", entry.service.Name)

	entry, ok, err = cs.updateLBQueue.pop()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "s1", entry.service.Name)

	_, ok, err = cs.updateLBQueue.pop()
	require.NoError(t, err)
	assert.False(t, ok)

	time.Sleep(2 * minRetryDelay)

	entry, ok, err = cs.updateLBQueue.pop()
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, "s2", entry.service.Name)
}

func Test_serviceNodeQueue_pop_race(t *testing.T) {
	cs, cleanup := preparePopTest(t)
	defer cleanup()

	err := cs.updateLBQueue.push(queueEntry{service: &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s3"},
	}, start: time.Now()})
	require.NoError(t, err)
	err = cs.updateLBQueue.push(queueEntry{service: &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s2"},
	}, start: time.Now()})
	require.NoError(t, err)
	err = cs.updateLBQueue.push(queueEntry{service: &v1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s1"},
	}, start: time.Now()})
	require.NoError(t, err)

	nGoroutines := 10

	entries := make(chan string, nGoroutines)
	wg := sync.WaitGroup{}
	for i := 0; i < nGoroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entry, ok, err := cs.updateLBQueue.pop()
			require.NoError(t, err)
			if ok {
				entries <- entry.service.Name
			}
		}()
	}

	wg.Wait()
	close(entries)
	var strEntries []string
	for e := range entries {
		strEntries = append(strEntries, e)
	}

	sort.Strings(strEntries)
	assert.Equal(t, []string{"s1", "s2", "s3"}, strEntries)
}

func Test_sortableQueueEntries(t *testing.T) {
	t0 := time.Now()
	entries := sortableQueueEntries{
		{
			queueEntry: queueEntry{
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s1"},
				},
				backoffUntil: t0.Add(200 * time.Millisecond),
				start:        t0,
			},
			topRevision: 3,
		},
		{
			queueEntry: queueEntry{
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s2"},
				},
				start: t0,
			},
			topRevision: 2,
		},
		{
			queueEntry: queueEntry{
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s3"},
				},
				start: t0,
			},
			topRevision: 1,
		},
		{
			queueEntry: queueEntry{
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s4"},
				},
				backoffUntil: t0.Add(200 * time.Millisecond),
				start:        t0.Add(-time.Second),
			},
			topRevision: 1,
		},
		{
			queueEntry: queueEntry{
				service: &v1.Service{
					ObjectMeta: metav1.ObjectMeta{Namespace: "ns1", Name: "s5"},
				},
				backoffUntil: t0.Add(time.Second),
				start:        t0.Add(-time.Second),
			},
			topRevision: 10,
		},
	}

	sort.Sort(entries)
	assert.Equal(t, []string{"s2", "s3", "s1", "s4", "s5"}, entriesToSvcNames(entries))

	time.Sleep(400 * time.Millisecond)
	sort.Sort(entries)
	assert.Equal(t, []string{"s1", "s2", "s4", "s3", "s5"}, entriesToSvcNames(entries))
}

func entriesToSvcNames(entries sortableQueueEntries) []string {
	var names []string
	for _, e := range entries {
		names = append(names, e.queueEntry.service.Name)
	}
	return names
}
