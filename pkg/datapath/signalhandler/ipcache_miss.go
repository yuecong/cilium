// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package signalhandler

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/cilium/hive/cell"
	"github.com/cilium/hive/job"

	"github.com/cilium/stream"

	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/signal"
)

// IPCacheMissData matches the BPF struct ipcache_miss_msg layout.
// Fields are in network byte order for dst_ip and native endian for cluster_id.
type IPCacheMissData struct {
	DstIP     uint32
	ClusterID uint16
	Pad       uint16
}

// String returns a low-cardinality string representation for metrics.
func (d IPCacheMissData) String() string {
	return "v4"
}

// IP returns the destination IP as a net.IP.
func (d IPCacheMissData) IP() net.IP {
	ip := make(net.IP, 4)
	binary.BigEndian.PutUint32(ip, d.DstIP)
	return ip
}

// IPCacheMissHandler processes ipcache miss signals from the BPF datapath.
// On receiving a signal, it attempts to resolve the destination IP by looking
// up the /32 entry from the appropriate remote cluster's kvstore cache and
// upserting it into the local BPF ipcache.
type IPCacheMissHandler struct {
	logger *slog.Logger

	// inflight tracks IPs that are currently being resolved to avoid
	// duplicate resolution attempts.
	inflight sync.Map

	// resolver is called to resolve an ipcache miss. It is pluggable
	// for testing purposes. In production, it queries the local kvstoremesh
	// etcd cache.
	resolver IPCacheMissResolver

	// deduplicationTTL is how long to keep the inflight entry after
	// resolution completes, suppressing duplicate signals for the same IP.
	deduplicationTTL time.Duration
}

// IPCacheMissResolver is the interface for resolving an ipcache miss.
type IPCacheMissResolver interface {
	// ResolveIPCacheMiss attempts to resolve the given IP address by looking
	// it up in the remote cluster's cached data and upserting the /32 entry
	// into the local BPF ipcache.
	ResolveIPCacheMiss(ctx context.Context, ip net.IP, clusterID uint16) error
}

// IPCacheMissConfig holds configuration for the ipcache miss handler.
type IPCacheMissConfig struct {
	// QueueSize is the size of the signal channel buffer.
	QueueSize int
	// DeduplicationTTL is how long to suppress duplicate signals for the same IP.
	DeduplicationTTL time.Duration
}

// DefaultIPCacheMissConfig returns the default configuration.
func DefaultIPCacheMissConfig() IPCacheMissConfig {
	return IPCacheMissConfig{
		QueueSize:        4096,
		DeduplicationTTL: 5 * time.Second,
	}
}

// RegisterIPCacheMissHandler registers the ipcache miss signal handler with
// the signal manager. This follows the same pattern as auth signal registration
// in pkg/auth/cell.go.
func RegisterIPCacheMissHandler(
	jg job.Group,
	sm signal.SignalManager,
	resolver IPCacheMissResolver,
	logger *slog.Logger,
	cfg IPCacheMissConfig,
) error {
	if resolver == nil {
		logger.Info("No ipcache miss resolver configured, skipping signal handler registration")
		return nil
	}

	handler := &IPCacheMissHandler{
		logger:           logger,
		resolver:         resolver,
		deduplicationTTL: cfg.DeduplicationTTL,
	}

	signalChannel := make(chan IPCacheMissData, cfg.QueueSize)

	if err := sm.RegisterHandler(signal.ChannelHandler(signalChannel), signal.SignalIPCacheMiss); err != nil {
		return fmt.Errorf("failed to register ipcache miss signal handler: %w", err)
	}

	jg.Add(job.Observer(
		"ipcache-miss-handler",
		handler.handleMiss,
		stream.FromChannel(signalChannel),
	))

	return nil
}

func (h *IPCacheMissHandler) handleMiss(ctx context.Context, data IPCacheMissData) error {
	ip := data.IP()
	ipStr := ip.String()

	// Deduplicate: skip if we're already resolving this IP.
	if _, loaded := h.inflight.LoadOrStore(ipStr, struct{}{}); loaded {
		return nil
	}

	// Resolve the miss asynchronously. Keep the inflight entry for the
	// deduplication TTL after resolution completes to suppress duplicate
	// signals for the same IP.
	go func() {
		resolveCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()

		if err := h.resolver.ResolveIPCacheMiss(resolveCtx, ip, data.ClusterID); err != nil {
			h.logger.Debug("Failed to resolve ipcache miss",
				logfields.IPAddr, ipStr,
				"clusterID", data.ClusterID,
				logfields.Error, err,
			)
		} else {
			h.logger.Debug("Resolved ipcache miss",
				logfields.IPAddr, ipStr,
				"clusterID", data.ClusterID,
			)
		}

		// Keep the inflight entry for the dedup TTL to suppress duplicate signals.
		time.Sleep(h.deduplicationTTL)
		h.inflight.Delete(ipStr)
	}()

	return nil
}

// Cell provides the ipcache miss signal handler as a Hive cell.
var Cell = cell.Module(
	"ipcache-miss-handler",
	"Handles BPF ipcache miss signals for on-demand endpoint resolution",

	cell.Provide(func() IPCacheMissConfig {
		return DefaultIPCacheMissConfig()
	}),

	cell.Invoke(registerIPCacheMissCell),
)

type ipcacheMissCellParams struct {
	cell.In

	JobGroup  job.Group
	SignalMgr signal.SignalManager
	Logger    *slog.Logger
	Config    IPCacheMissConfig
	Resolver  IPCacheMissResolver `optional:"true"`
}

func registerIPCacheMissCell(params ipcacheMissCellParams) error {
	return RegisterIPCacheMissHandler(
		params.JobGroup,
		params.SignalMgr,
		params.Resolver,
		params.Logger,
		params.Config,
	)
}
