// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package kvstoremesh

import (
	"encoding/json"
	"sync"

	"github.com/sirupsen/logrus"

	"github.com/cilium/cilium/pkg/identity"
	"github.com/cilium/cilium/pkg/kvstore/store"
	nodeTypes "github.com/cilium/cilium/pkg/node/types"
)

const (
	// topologyRegionLabel is the well-known Kubernetes node label for the
	// failure-domain region.
	topologyRegionLabel = "topology.kubernetes.io/region"
)

// NodeRegionCache maintains a mapping from node IP to region, populated from
// CiliumNode objects that flow through the nodes reflector.
type NodeRegionCache struct {
	mu      sync.RWMutex
	regions map[string]string // nodeIP -> region
	logger  logrus.FieldLogger
}

// NewNodeRegionCache creates a new NodeRegionCache.
func NewNodeRegionCache(logger logrus.FieldLogger) *NodeRegionCache {
	return &NodeRegionCache{
		regions: make(map[string]string),
		logger:  logger,
	}
}

// Update processes a CiliumNode key/value and updates the cache.
func (c *NodeRegionCache) Update(key store.Key) {
	kv, ok := key.(*store.KVPair)
	if !ok {
		return
	}

	var node nodeTypes.Node
	if err := json.Unmarshal(kv.Value, &node); err != nil {
		return
	}

	region := node.Labels[topologyRegionLabel]
	if region == "" {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Index by all known IPs of this node.
	for _, addr := range node.IPAddresses {
		c.regions[addr.IP.String()] = region
	}
}

// Delete removes a node from the cache.
func (c *NodeRegionCache) Delete(key store.Key) {
	kv, ok := key.(*store.KVPair)
	if !ok {
		return
	}

	var node nodeTypes.Node
	if err := json.Unmarshal(kv.Value, &node); err != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	for _, addr := range node.IPAddresses {
		delete(c.regions, addr.IP.String())
	}
}

// RegionForHost returns the region for a node identified by its IP.
func (c *NodeRegionCache) RegionForHost(hostIP string) (string, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	r, ok := c.regions[hostIP]
	return r, ok
}

// NewRegionalEndpointFilter returns a filter function suitable for use with
// the ipcache syncer. Entries with syncScope=regional are only accepted if
// the HostIP belongs to a node in the same region as localRegion. Entries
// without a syncScope (or with syncScope=global) are always accepted.
func NewRegionalEndpointFilter(localRegion string, cache *NodeRegionCache, logger logrus.FieldLogger) func(store.Key) bool {
	return func(key store.Key) bool {
		kv, ok := key.(*store.KVPair)
		if !ok {
			return true
		}

		var pair identity.IPIdentityPair
		if err := json.Unmarshal(kv.Value, &pair); err != nil {
			return true // can't parse, let it through
		}

		if pair.SyncScope != "regional" {
			return true
		}

		// Regional entry — check whether the host is in our region.
		if pair.HostIP == nil {
			return true
		}
		hostRegion, found := cache.RegionForHost(pair.HostIP.String())
		if !found {
			// Host region unknown; accept to be safe.
			logger.WithFields(logrus.Fields{
				"hostIP":    pair.HostIP,
				"syncScope": pair.SyncScope,
			}).Debug("Host region unknown for regional endpoint, accepting")
			return true
		}
		if hostRegion != localRegion {
			logger.WithFields(logrus.Fields{
				"hostIP":      pair.HostIP,
				"hostRegion":  hostRegion,
				"localRegion": localRegion,
			}).Debug("Filtering regional endpoint from different region")
			return false
		}
		return true
	}
}
