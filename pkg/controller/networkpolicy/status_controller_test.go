// Copyright 2020 Antrea Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package networkpolicy

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/util/workqueue"

	"antrea.io/antrea/pkg/apis/controlplane"
	crdv1alpha1 "antrea.io/antrea/pkg/apis/crd/v1alpha1"
	"antrea.io/antrea/pkg/apiserver/storage"
	antreaclientset "antrea.io/antrea/pkg/client/clientset/versioned"
	antreafakeclientset "antrea.io/antrea/pkg/client/clientset/versioned/fake"
	antreainformers "antrea.io/antrea/pkg/client/informers/externalversions"
	"antrea.io/antrea/pkg/controller/networkpolicy/store"
	"antrea.io/antrea/pkg/controller/types"
)

type fakeNetworkPolicyControl struct {
	sync.Mutex
	anpStatus *crdv1alpha1.NetworkPolicyStatus
	cnpStatus *crdv1alpha1.NetworkPolicyStatus
}

func (c *fakeNetworkPolicyControl) UpdateAntreaNetworkPolicyStatus(namespace, name string, status *crdv1alpha1.NetworkPolicyStatus) error {
	c.Lock()
	defer c.Unlock()
	c.anpStatus = status
	return nil
}

func (c *fakeNetworkPolicyControl) UpdateAntreaClusterNetworkPolicyStatus(name string, status *crdv1alpha1.NetworkPolicyStatus) error {
	c.Lock()
	defer c.Unlock()
	c.cnpStatus = status
	return nil
}

func (c *fakeNetworkPolicyControl) getAntreaNetworkPolicyStatus() *crdv1alpha1.NetworkPolicyStatus {
	c.Lock()
	defer c.Unlock()
	return c.anpStatus
}

func (c *fakeNetworkPolicyControl) getAntreaClusterNetworkPolicyStatus() *crdv1alpha1.NetworkPolicyStatus {
	c.Lock()
	defer c.Unlock()
	return c.cnpStatus
}

func newTestStatusController(initialObjects ...runtime.Object) (*StatusController, antreaclientset.Interface, antreainformers.SharedInformerFactory, storage.Interface, *fakeNetworkPolicyControl) {
	// clientset := fake.NewSimpleClientset(initialObjects...)
	networkPolicyStore := store.NewNetworkPolicyStore()
	antreaClientset := antreafakeclientset.NewSimpleClientset(initialObjects...)
	antreaInformerFactory := antreainformers.NewSharedInformerFactory(antreaClientset, 0)
	networkPolicyControl := &fakeNetworkPolicyControl{}

	cnpInformer := antreaInformerFactory.Crd().V1alpha1().ClusterNetworkPolicies()
	anpInformer := antreaInformerFactory.Crd().V1alpha1().NetworkPolicies()
	statusController := &StatusController{
		npControlInterface:         networkPolicyControl,
		queue:                      workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "networkpolicy"),
		internalNetworkPolicyStore: networkPolicyStore,
		statuses:                   map[string]map[string]*controlplane.NetworkPolicyNodeStatus{},
		cnpListerSynced:            cnpInformer.Informer().HasSynced,
		anpListerSynced:            anpInformer.Informer().HasSynced,
	}
	return statusController, antreaClientset, antreaInformerFactory, networkPolicyStore, networkPolicyControl
}

func newInternalNetworkPolicy(name string, generation int64, nodes []string, ref *controlplane.NetworkPolicyReference) *types.NetworkPolicy {
	return &types.NetworkPolicy{
		SpanMeta:   types.SpanMeta{NodeNames: sets.New[string](nodes...)},
		Generation: generation,
		Name:       name,
		SourceRef:  ref,
	}
}

func newNetworkPolicyStatus(name string, nodeName string, generation int64, errorMessage string) *controlplane.NetworkPolicyStatus {
	failed := false
	if errorMessage != "" {
		failed = true
	}
	return &controlplane.NetworkPolicyStatus{
		ObjectMeta: v1.ObjectMeta{
			Name: name,
		},
		Nodes: []controlplane.NetworkPolicyNodeStatus{
			{
				NodeName:           nodeName,
				Generation:         generation,
				RealizationFailure: failed,
				Message:            errorMessage,
			},
		},
	}
}

func toAntreaNetworkPolicy(inp *types.NetworkPolicy) runtime.Object {
	if inp.SourceRef.Type == controlplane.AntreaNetworkPolicy {
		return &crdv1alpha1.NetworkPolicy{
			ObjectMeta: v1.ObjectMeta{
				Namespace:  inp.SourceRef.Namespace,
				Name:       inp.SourceRef.Name,
				Generation: inp.Generation,
			},
		}
	}
	return &crdv1alpha1.ClusterNetworkPolicy{
		ObjectMeta: v1.ObjectMeta{
			Name:       inp.SourceRef.Name,
			Generation: inp.Generation,
		},
	}
}

func newAntreaNetworkPolicyReference(namespace, name string) *controlplane.NetworkPolicyReference {
	return &controlplane.NetworkPolicyReference{
		Type:      controlplane.AntreaNetworkPolicy,
		Namespace: namespace,
		Name:      name,
	}
}

func newAntreaClusterNetworkPolicyReference(name string) *controlplane.NetworkPolicyReference {
	return &controlplane.NetworkPolicyReference{
		Type: controlplane.AntreaClusterNetworkPolicy,
		Name: name,
	}
}

func generateRealizationFailureConditions(failedNodeCount int, failedNodeDetails string) []crdv1alpha1.NetworkPolicyCondition {
	conditions := GenerateNetworkPolicyCondition(nil)
	failureMessage := fmt.Sprintf("Failed Nodes count %d: %s", failedNodeCount, failedNodeDetails)
	conditions = append(conditions, crdv1alpha1.NetworkPolicyCondition{
		Type:               crdv1alpha1.NetworkPolicyConditionRealizationFailure,
		Status:             v1.ConditionTrue,
		LastTransitionTime: v1.Now(),
		Reason:             "NetworkPolicyRealizationFailedOnNode",
		Message:            failureMessage,
	})
	return conditions
}

func TestCreateAntreaNetworkPolicy(t *testing.T) {
	tests := []struct {
		name                         string
		networkPolicy                []*types.NetworkPolicy
		collectedNetworkPolicyStatus []*controlplane.NetworkPolicyStatus
		expectedANPStatus            *crdv1alpha1.NetworkPolicyStatus
		expectedCNPStatus            *crdv1alpha1.NetworkPolicyStatus
	}{
		{
			name: "no realization status",
			networkPolicy: []*types.NetworkPolicy{
				newInternalNetworkPolicy("anp1", 1, []string{"node1", "node2"}, newAntreaNetworkPolicyReference("ns1", "anp1")),
				newInternalNetworkPolicy("cnp1", 1, []string{"node1", "node2"}, newAntreaClusterNetworkPolicyReference("cnp1")),
			},
			expectedANPStatus: &crdv1alpha1.NetworkPolicyStatus{
				Phase:                crdv1alpha1.NetworkPolicyRealizing,
				ObservedGeneration:   1,
				CurrentNodesRealized: 0,
				DesiredNodesRealized: 2,
				Conditions:           GenerateNetworkPolicyCondition(nil),
			},
			expectedCNPStatus: &crdv1alpha1.NetworkPolicyStatus{
				Phase:                crdv1alpha1.NetworkPolicyRealizing,
				ObservedGeneration:   1,
				CurrentNodesRealized: 0,
				DesiredNodesRealized: 2,
				Conditions:           GenerateNetworkPolicyCondition(nil),
			},
		},
		{
			name: "partially realized",
			networkPolicy: []*types.NetworkPolicy{
				newInternalNetworkPolicy("anp1", 2, []string{"node1", "node2"}, newAntreaNetworkPolicyReference("ns1", "anp1")),
				newInternalNetworkPolicy("cnp1", 3, []string{"node1", "node2"}, newAntreaClusterNetworkPolicyReference("cnp1")),
			},
			collectedNetworkPolicyStatus: []*controlplane.NetworkPolicyStatus{
				newNetworkPolicyStatus("anp1", "node1", 1, ""),
				newNetworkPolicyStatus("anp1", "node2", 2, ""),
				newNetworkPolicyStatus("cnp1", "node1", 2, ""),
				newNetworkPolicyStatus("cnp1", "node2", 3, ""),
			},
			expectedANPStatus: &crdv1alpha1.NetworkPolicyStatus{
				Phase:                crdv1alpha1.NetworkPolicyRealizing,
				ObservedGeneration:   2,
				CurrentNodesRealized: 1,
				DesiredNodesRealized: 2,
				Conditions:           GenerateNetworkPolicyCondition(nil),
			},
			expectedCNPStatus: &crdv1alpha1.NetworkPolicyStatus{
				Phase:                crdv1alpha1.NetworkPolicyRealizing,
				ObservedGeneration:   3,
				CurrentNodesRealized: 1,
				DesiredNodesRealized: 2,
				Conditions:           GenerateNetworkPolicyCondition(nil),
			},
		},
		{
			name: "entirely realized",
			networkPolicy: []*types.NetworkPolicy{
				newInternalNetworkPolicy("anp1", 3, []string{"node1", "node2"}, newAntreaNetworkPolicyReference("ns1", "anp1")),
				newInternalNetworkPolicy("cnp1", 4, []string{"node1", "node2"}, newAntreaClusterNetworkPolicyReference("cnp1")),
			},
			collectedNetworkPolicyStatus: []*controlplane.NetworkPolicyStatus{
				newNetworkPolicyStatus("anp1", "node1", 3, ""),
				newNetworkPolicyStatus("anp1", "node2", 3, ""),
				newNetworkPolicyStatus("cnp1", "node1", 4, ""),
				newNetworkPolicyStatus("cnp1", "node2", 4, ""),
			},
			expectedANPStatus: &crdv1alpha1.NetworkPolicyStatus{
				Phase:                crdv1alpha1.NetworkPolicyRealized,
				ObservedGeneration:   3,
				CurrentNodesRealized: 2,
				DesiredNodesRealized: 2,
				Conditions:           GenerateNetworkPolicyCondition(nil),
			},
			expectedCNPStatus: &crdv1alpha1.NetworkPolicyStatus{
				Phase:                crdv1alpha1.NetworkPolicyRealized,
				ObservedGeneration:   4,
				CurrentNodesRealized: 2,
				DesiredNodesRealized: 2,
				Conditions:           GenerateNetworkPolicyCondition(nil),
			},
		},
		{
			name: "failed realized",
			networkPolicy: []*types.NetworkPolicy{
				newInternalNetworkPolicy("anp1", 4, []string{"node1", "node2"}, newAntreaNetworkPolicyReference("ns1", "anp1")),
				newInternalNetworkPolicy("cnp1", 5, []string{"node1", "node2"}, newAntreaClusterNetworkPolicyReference("cnp1")),
			},
			collectedNetworkPolicyStatus: []*controlplane.NetworkPolicyStatus{
				newNetworkPolicyStatus("anp1", "node1", 4, "agent failure"),
				newNetworkPolicyStatus("anp1", "node2", 4, ""),
				newNetworkPolicyStatus("cnp1", "node1", 5, "agent failure"),
				newNetworkPolicyStatus("cnp1", "node2", 5, "agent crash"),
			},
			expectedANPStatus: &crdv1alpha1.NetworkPolicyStatus{
				Phase:                crdv1alpha1.NetworkPolicyFailed,
				ObservedGeneration:   4,
				CurrentNodesRealized: 1,
				DesiredNodesRealized: 2,
				Conditions:           generateRealizationFailureConditions(1, `"node1":"agent failure"`),
			},
			expectedCNPStatus: &crdv1alpha1.NetworkPolicyStatus{
				Phase:                crdv1alpha1.NetworkPolicyFailed,
				ObservedGeneration:   5,
				CurrentNodesRealized: 0,
				DesiredNodesRealized: 2,
				Conditions:           generateRealizationFailureConditions(2, `"node1":"agent failure"...`),
			},
		},
	}
	updateMaxConditionMessageLength := func() func() {
		originalMaxConditionMessageLength := maxConditionMessageLength
		maxConditionMessageLength = 45
		return func() {
			maxConditionMessageLength = originalMaxConditionMessageLength
		}
	}
	defer updateMaxConditionMessageLength()()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var initObjects []runtime.Object
			for _, policy := range tt.networkPolicy {
				initObjects = append(initObjects, toAntreaNetworkPolicy(policy))
			}
			statusController, _, antreaInformerFactory, networkPolicyStore, networkPolicyControl := newTestStatusController(initObjects...)
			stopCh := make(chan struct{})
			defer close(stopCh)
			antreaInformerFactory.Start(stopCh)
			go statusController.Run(stopCh)

			for _, policy := range tt.networkPolicy {
				networkPolicyStore.Create(policy)
			}
			for _, status := range tt.collectedNetworkPolicyStatus {
				statusController.UpdateStatus(status)
			}

			// TODO: Use a determinate mechanism.
			time.Sleep(500 * time.Millisecond)
			assert.True(t, NetworkPolicyStatusEqual(*tt.expectedANPStatus, *networkPolicyControl.getAntreaNetworkPolicyStatus()))
			assert.True(t, NetworkPolicyStatusEqual(*tt.expectedCNPStatus, *networkPolicyControl.getAntreaClusterNetworkPolicyStatus()))
		})
	}
}

func TestUpdateAntreaNetworkPolicy(t *testing.T) {
	anp1 := newInternalNetworkPolicy("anp1", 1, []string{"node1", "node2"}, newAntreaNetworkPolicyReference("ns1", "anp1"))
	cnp1 := newInternalNetworkPolicy("cnp1", 2, []string{"node3", "node4", "node5"}, newAntreaClusterNetworkPolicyReference("cnp1"))
	statusController, _, antreaInformerFactory, networkPolicyStore, networkPolicyControl := newTestStatusController(toAntreaNetworkPolicy(anp1), toAntreaNetworkPolicy(cnp1))
	stopCh := make(chan struct{})
	defer close(stopCh)
	antreaInformerFactory.Start(stopCh)
	go statusController.Run(stopCh)

	networkPolicyStore.Create(anp1)
	networkPolicyStore.Create(cnp1)
	statusController.UpdateStatus(newNetworkPolicyStatus("anp1", "node1", 1, ""))
	statusController.UpdateStatus(newNetworkPolicyStatus("anp1", "node2", 1, ""))
	statusController.UpdateStatus(newNetworkPolicyStatus("cnp1", "node3", 2, ""))
	statusController.UpdateStatus(newNetworkPolicyStatus("cnp1", "node4", 2, ""))
	statusController.UpdateStatus(newNetworkPolicyStatus("cnp1", "node5", 2, ""))
	// TODO: Use a determinate mechanism.
	time.Sleep(500 * time.Millisecond)
	assert.True(t, NetworkPolicyStatusEqual(crdv1alpha1.NetworkPolicyStatus{
		Phase:                crdv1alpha1.NetworkPolicyRealized,
		ObservedGeneration:   1,
		CurrentNodesRealized: 2,
		DesiredNodesRealized: 2,
		Conditions:           GenerateNetworkPolicyCondition(nil),
	}, *networkPolicyControl.getAntreaNetworkPolicyStatus()))
	assert.True(t, NetworkPolicyStatusEqual(crdv1alpha1.NetworkPolicyStatus{
		Phase:                crdv1alpha1.NetworkPolicyRealized,
		ObservedGeneration:   2,
		CurrentNodesRealized: 3,
		DesiredNodesRealized: 3,
		Conditions:           GenerateNetworkPolicyCondition(nil),
	}, *networkPolicyControl.getAntreaClusterNetworkPolicyStatus()))

	anp1Updated := newInternalNetworkPolicy("anp1", 2, []string{"node1", "node2", "node3"}, newAntreaNetworkPolicyReference("ns1", "anp1"))
	cnp1Updated := newInternalNetworkPolicy("cnp1", 3, []string{"node4", "node5"}, newAntreaClusterNetworkPolicyReference("cnp1"))
	networkPolicyStore.Update(anp1Updated)
	networkPolicyStore.Update(cnp1Updated)
	// TODO: Use a determinate mechanism.
	time.Sleep(500 * time.Millisecond)
	assert.True(t, NetworkPolicyStatusEqual(crdv1alpha1.NetworkPolicyStatus{
		Phase:                crdv1alpha1.NetworkPolicyRealizing,
		ObservedGeneration:   2,
		CurrentNodesRealized: 0,
		DesiredNodesRealized: 3,
		Conditions:           GenerateNetworkPolicyCondition(nil),
	}, *networkPolicyControl.getAntreaNetworkPolicyStatus()))
	assert.True(t, NetworkPolicyStatusEqual(crdv1alpha1.NetworkPolicyStatus{
		Phase:                crdv1alpha1.NetworkPolicyRealizing,
		ObservedGeneration:   3,
		CurrentNodesRealized: 0,
		DesiredNodesRealized: 2,
		Conditions:           GenerateNetworkPolicyCondition(nil),
	}, *networkPolicyControl.getAntreaClusterNetworkPolicyStatus()))
}

func TestDeleteAntreaNetworkPolicy(t *testing.T) {
	initialNetworkPolicy := newInternalNetworkPolicy("anp1", 1, []string{"node1", "node2"}, newAntreaNetworkPolicyReference("ns1", "anp1"))
	initialANP := toAntreaNetworkPolicy(initialNetworkPolicy)
	statusController, _, antreaInformerFactory, networkPolicyStore, _ := newTestStatusController(initialANP)
	stopCh := make(chan struct{})
	defer close(stopCh)
	antreaInformerFactory.Start(stopCh)
	go statusController.Run(stopCh)

	networkPolicyStore.Create(initialNetworkPolicy)
	statuses := []*controlplane.NetworkPolicyStatus{
		newNetworkPolicyStatus("anp1", "node1", 1, ""),
		newNetworkPolicyStatus("anp1", "node2", 1, ""),
	}
	for _, status := range statuses {
		statusController.UpdateStatus(status)
	}
	assert.Equal(t, 2, len(statusController.getNodeStatuses(initialNetworkPolicy.Name)))

	networkPolicyStore.Delete(initialNetworkPolicy.Name)
	// TODO: Use a determinate mechanism.
	time.Sleep(500 * time.Millisecond)
	assert.Empty(t, statusController.getNodeStatuses(initialNetworkPolicy.Name))
}

// BenchmarkSyncHandler benchmarks syncHandler when the policy spans 1000 Nodes. Its current result is:
// 70024 ns/op            8338 B/op          8 allocs/op
func BenchmarkSyncHandler(b *testing.B) {
	nodeNum := 1000
	nodes := make([]string, 0, nodeNum)
	for i := 0; i < nodeNum; i++ {
		nodes = append(nodes, fmt.Sprintf("node%d", i))
	}
	networkPolicy := newInternalNetworkPolicy("anp1", 1, nodes, newAntreaNetworkPolicyReference("ns1", "anp1"))
	statusController, _, _, networkPolicyStore, _ := newTestStatusController()

	networkPolicyStore.Create(networkPolicy)
	for _, node := range nodes {
		statusController.UpdateStatus(newNetworkPolicyStatus("anp1", node, 1, ""))
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		statusController.syncHandler("anp1")
	}
}
