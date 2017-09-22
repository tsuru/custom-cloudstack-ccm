package cloudstack

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/pkg/api/v1"
)

func TestFilterNodesMatchingLabels(t *testing.T) {
	nodes := []*v1.Node{
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pool": "pool1"}}},
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pool": "pool2"}}},
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"pool": "pool2"}}},
		{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{}}},
	}
	s1 := v1.Service{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app-pool": "pool1"}}}
	s2 := v1.Service{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app-pool": "pool2"}}}
	tt := []struct {
		name          string
		cs            CSCloud
		service       v1.Service
		expectedNodes []*v1.Node
	}{
		{"matchSingle", CSCloud{serviceLabel: "app-pool", nodeLabel: "pool"}, s1, nodes[0:1]},
		{"emptyLabels", CSCloud{}, s1, nodes},
		{"matchMultiple", CSCloud{serviceLabel: "app-pool", nodeLabel: "pool"}, s2, nodes[1:3]},
	}
	for _, tc := range tt {
		t.Run(tc.name, func(t *testing.T) {
			filtered := tc.cs.filterNodesMatchingLabels(nodes, tc.service)
			if !reflect.DeepEqual(filtered, tc.expectedNodes) {
				t.Errorf("Expected %+v. Got %+v.", tc.expectedNodes, filtered)
			}
		})
	}
}
