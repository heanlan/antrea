// Copyright 2021 Antrea Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package connections

import (
	"fmt"
	"time"

	"k8s.io/klog/v2"

	"antrea.io/antrea/pkg/agent/flowexporter"
	"antrea.io/antrea/pkg/agent/flowexporter/priorityqueue"
	"antrea.io/antrea/pkg/agent/interfacestore"
	"antrea.io/antrea/pkg/agent/metrics"
	"antrea.io/antrea/pkg/agent/proxy"
	"antrea.io/antrea/pkg/util/ip"
)

type DenyConnectionStore struct {
	connectionStore
}

func NewDenyConnectionStore(ifaceStore interfacestore.InterfaceStore, proxier proxy.Proxier, o *flowexporter.FlowExporterOptions) *DenyConnectionStore {
	return &DenyConnectionStore{
		connectionStore: NewConnectionStore(ifaceStore, proxier, o),
	}
}

func (ds *DenyConnectionStore) RunPeriodicDeletion(stopCh <-chan struct{}) {
	pollTicker := time.NewTicker(periodicDeleteInterval)
	defer pollTicker.Stop()

	for {
		select {
		case <-stopCh:
			break
		case <-pollTicker.C:
			deleteIfStaleConn := func(key flowexporter.ConnectionKey, conn *flowexporter.Connection) error {
				if conn.ReadyToDelete || time.Since(conn.LastExportTime) >= ds.staleConnectionTimeout {
					if err := ds.deleteConnWithoutLock(key); err != nil {
						return err
					}
					klog.Infof("DELETE IF STALE CONN, connKey: %s, conn: %p", key, conn)
					klog.Info("\n")
					for k, v := range ds.connectionStore.Connections {
						klog.InfoS("deny conn store", "key", k)
						klog.Infof("deny conn store, conn: %p", v)
						klog.InfoS("deny conn store", "flowkey", v.FlowKey)
						klog.InfoS("deny conn store", "val", v)
						klog.Info("\n")
					}
				}
				return nil
			}
			klog.Info("acquire ds lock")
			klog.Info("\n")
			ds.ForAllConnectionsDo(deleteIfStaleConn)
			klog.Info("release ds lock")
			klog.Info("\n")
			klog.V(2).Infof("Stale connections in the Deny Connection Store are successfully deleted.")
		}
	}
}

// AddOrUpdateConn updates the connection if it is already present, i.e., update timestamp, counters etc.,
// or adds a new connection with the resolved K8s metadata.
func (ds *DenyConnectionStore) AddOrUpdateConn(conn *flowexporter.Connection, timeSeen time.Time, bytes uint64) {
	connKey := flowexporter.NewConnectionKey(conn)
	klog.Infof("ADD OR UPDATE, connKey: %s, conn: %p, flowKey: %v", connKey, conn, conn.FlowKey)
	klog.Info("\n")
	ds.mutex.Lock()
	defer ds.mutex.Unlock()

	if _, exist := ds.Connections[connKey]; exist {
		if conn.ReadyToDelete {
			klog.Infof("conn is readyToDelete, skip update, connKey: %s, conn: %p", connKey, conn)
			klog.Info("\n")
			return
		}
		conn.OriginalBytes += bytes
		conn.OriginalPackets += 1
		conn.StopTime = timeSeen
		conn.IsActive = true
		existingItem, exists := ds.expirePriorityQueue.KeyToItem[connKey]
		if !exists {
			klog.Infof("conn does not exist in KeyToItem map, connKey: %s, conn: %p", connKey, conn)
			klog.Info("\n")
			ds.expirePriorityQueue.AddItemToQueue(connKey, conn)
		} else {
			klog.Infof("update existing conn in PQ, connKey: %s, conn: %p", connKey, conn)
			klog.Info("\n")
			ds.connectionStore.expirePriorityQueue.Update(existingItem, existingItem.ActiveExpireTime,
				time.Now().Add(ds.connectionStore.expirePriorityQueue.IdleFlowTimeout))
		}
		klog.V(4).InfoS("Deny connection has been updated", "connection", conn)
	} else {
		conn.StartTime = timeSeen
		conn.StopTime = timeSeen
		conn.OriginalBytes = bytes
		conn.OriginalPackets = uint64(1)
		ds.fillPodInfo(conn)
		protocolStr := ip.IPProtocolNumberToString(conn.FlowKey.Protocol, "UnknownProtocol")
		serviceStr := fmt.Sprintf("%s:%d/%s", conn.DestinationServiceAddress, conn.DestinationServicePort, protocolStr)
		ds.fillServiceInfo(conn, serviceStr)
		metrics.TotalDenyConnections.Inc()
		conn.IsActive = true
		ds.Connections[connKey] = conn
		ds.expirePriorityQueue.AddItemToQueue(connKey, conn)
		klog.V(4).InfoS("New deny connection added", "connection", conn)
		klog.InfoS("add new conn, connKey: %s, conn: %p", connKey, conn)
		klog.Info("\n")
	}
}

func (ds *DenyConnectionStore) GetExpiredConns(expiredConns []flowexporter.Connection, currTime time.Time, maxSize int) ([]flowexporter.Connection, time.Duration) {
	ds.AcquireConnStoreLock()
	defer ds.ReleaseConnStoreLock()
	for i := 0; i < maxSize; i++ {
		pqItem := ds.connectionStore.expirePriorityQueue.GetTopExpiredItem(currTime)
		if pqItem == nil {
			break
		}
		expiredConns = append(expiredConns, *pqItem.Conn)

		if pqItem.IdleExpireTime.Before(currTime) {
			// If a deny connection item is idle time out, we set the ReadyToDelete
			// flag to true to do the deletion later.
			// klog.InfoS("readyToDelete", "conn", pqItem.Conn.FlowKey, "idle", pqItem.IdleExpireTime, "curr", currTime)
			// klog.Infof("readyToDelete, conn: %p", pqItem.Conn)
			// klog.Infof("pqItem: %p", pqItem)
			// klog.Info("\n")
			pqItem.Conn.ReadyToDelete = true
		}
		if pqItem.Conn.OriginalPackets <= pqItem.Conn.PrevPackets {
			// If a deny connection doesn't have increase in packet count,
			// we consider the connection to be inactive.
			pqItem.Conn.IsActive = false
			// klog.Infof("item no longer active: %p\n", pqItem)
		}
		ds.UpdateConnAndQueue(pqItem, currTime)

		// if pqItem.Conn.ReadyToDelete == true || pqItem.Conn.IsActive == false {
		// 	klog.Infof("remove item from map: %p\n", pqItem)
		// }
		for k, v := range ds.connectionStore.Connections {
			klog.InfoS("deny conn store", "key", k)
			klog.Infof("deny conn store, conn: %p", v)
			klog.InfoS("deny conn store", "flowkey", v.FlowKey)
			klog.InfoS("deny conn store", "val", v)
			klog.Info("\n")
		}
	}
	return expiredConns, ds.connectionStore.expirePriorityQueue.GetExpiryFromExpirePriorityQueue()
}

// deleteConnWithoutLock deletes the connection from the connection map given
// the connection key without grabbing the lock. Caller is expected to grab lock.
func (ds *DenyConnectionStore) deleteConnWithoutLock(connKey flowexporter.ConnectionKey) error {
	_, exists := ds.Connections[connKey]
	if !exists {
		return fmt.Errorf("connection with key %v doesn't exist in map", connKey)
	}
	delete(ds.Connections, connKey)
	metrics.TotalDenyConnections.Dec()
	return nil
}

func (ds *DenyConnectionStore) GetPriorityQueue() *priorityqueue.ExpirePriorityQueue {
	return ds.connectionStore.expirePriorityQueue
}
