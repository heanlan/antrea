// Copyright 2021 Antrea Authors
//
// Licensed under the Apache License, Version 2.0 (the “License”);
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an “AS IS” BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
package priorityqueue

import (
	"container/heap"
	"time"

	"antrea.io/antrea/pkg/agent/flowexporter"
)

// minExpiryTime provides two usages: 1. We want to avoid passing a non positive
// value to ticker 2. We want to avoid processing a single expired item per call.
// If multiple items have very close expiry time(<100ms), by adding a small constant,
// we can make these items expired and process in one call.
const minExpiryTime = 100 * time.Millisecond

type ExpirePriorityQueue struct {
	Items             []*flowexporter.ItemToExpire
	ActiveFlowTimeout time.Duration
	IdleFlowTimeout   time.Duration
	KeyToItem         map[flowexporter.ConnectionKey]*flowexporter.ItemToExpire
}

func NewExpirePriorityQueue(activeFlowTimeout time.Duration, idleFlowTimeout time.Duration) *ExpirePriorityQueue {
	return &ExpirePriorityQueue{
		Items:             make([]*flowexporter.ItemToExpire, 0),
		ActiveFlowTimeout: activeFlowTimeout,
		IdleFlowTimeout:   idleFlowTimeout,
		KeyToItem:         make(map[flowexporter.ConnectionKey]*flowexporter.ItemToExpire),
	}
}

func (pq *ExpirePriorityQueue) Len() int {
	return len(pq.Items)
}

func (pq *ExpirePriorityQueue) minExpireTime(i int) time.Time {
	if pq.Items[i].ActiveExpireTime.Before(pq.Items[i].IdleExpireTime) {
		return pq.Items[i].ActiveExpireTime
	} else {
		return pq.Items[i].IdleExpireTime
	}
}

func (pq *ExpirePriorityQueue) Less(i, j int) bool {
	return pq.minExpireTime(i).Before(pq.minExpireTime(j))
}

func (pq *ExpirePriorityQueue) Swap(i, j int) {
	pq.Items[i], pq.Items[j] = pq.Items[j], pq.Items[i]
	pq.Items[i].Index = i
	pq.Items[j].Index = j
}

func (pq *ExpirePriorityQueue) Push(x interface{}) {
	n := len((*pq).Items)
	item := x.(*flowexporter.ItemToExpire)
	item.Index = n
	(*pq).Items = append((*pq).Items, item)
}

func (pq *ExpirePriorityQueue) Pop() interface{} {
	n := len((*pq).Items)
	item := ((*pq).Items)[n-1]
	item.Index = -1
	(*pq).Items = ((*pq).Items)[0:(n - 1)]
	return item
}

// Peek returns the item at the beginning of the queue, without removing the
// item or otherwise mutating the queue. It is safe to call directly.
func (pq *ExpirePriorityQueue) Peek() *flowexporter.ItemToExpire {
	if pq.Len() == 0 {
		return nil
	}
	return pq.Items[0]
}

// Update modifies the priority of an Item in the queue.
func (pq *ExpirePriorityQueue) Update(item *flowexporter.ItemToExpire, activeExpireTime time.Time, idleExpireTime time.Time) {
	item.ActiveExpireTime = activeExpireTime
	item.IdleExpireTime = idleExpireTime

	heap.Fix(pq, item.Index)
}

// GetExpiryFromExpirePriorityQueue returns the shortest expire time duration
// from expire priority queue.
func (pq *ExpirePriorityQueue) GetExpiryFromExpirePriorityQueue() time.Duration {
	currTime := time.Now()
	if pq.Len() > 0 {
		// Get the minExpireTime of the top item in ExpirePriorityQueue.
		expiryDuration := minExpiryTime + pq.minExpireTime(0).Sub(currTime)
		if expiryDuration < 0 {
			return minExpiryTime
		}
		return expiryDuration
	}
	if pq.ActiveFlowTimeout < pq.IdleFlowTimeout {
		return pq.ActiveFlowTimeout
	}
	return pq.IdleFlowTimeout
}

func (pq *ExpirePriorityQueue) AddItemToQueue(connKey flowexporter.ConnectionKey, conn *flowexporter.Connection) {
	currTime := time.Now()
	pqItem := &flowexporter.ItemToExpire{
		Conn:             conn,
		ActiveExpireTime: currTime.Add(pq.ActiveFlowTimeout),
		IdleExpireTime:   currTime.Add(pq.IdleFlowTimeout),
	}
	heap.Push(pq, pqItem)
	pq.KeyToItem[connKey] = pqItem
}

func (pq *ExpirePriorityQueue) ResetActiveExpireTimeAndPush(pqItem *flowexporter.ItemToExpire, currTime time.Time) {
	pqItem.ActiveExpireTime = currTime.Add(pq.ActiveFlowTimeout)
	heap.Push(pq, pqItem)
}

func (pq *ExpirePriorityQueue) RemoveItemFromMap(conn *flowexporter.Connection) {
	connKey := flowexporter.NewConnectionKey(conn)
	delete(pq.KeyToItem, connKey)
}

func (pq *ExpirePriorityQueue) GetTopExpiredItem(currTime time.Time) *flowexporter.ItemToExpire {
	// If the queue is empty or top item is not timeout, then we do not have to
	// check the following items.
	topItem := pq.Peek()
	if topItem == nil || (topItem.ActiveExpireTime.After(currTime) && topItem.IdleExpireTime.After(currTime)) {
		return nil
	}
	pqItem := heap.Pop(pq).(*flowexporter.ItemToExpire)
	return pqItem
}
