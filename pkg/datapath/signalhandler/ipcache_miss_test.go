// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package signalhandler

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIPCacheMissData_IP(t *testing.T) {
	// 10.0.0.1 = 0x0a000001 in big-endian
	data := IPCacheMissData{
		DstIP:     0x0a000001,
		ClusterID: 5,
	}

	ip := data.IP()
	assert.Equal(t, net.ParseIP("10.0.0.1").To4(), ip)
}

func TestIPCacheMissData_String(t *testing.T) {
	data := IPCacheMissData{DstIP: 0x0a000001}
	assert.Equal(t, "v4", data.String())
}

func testLogger() *slog.Logger {
	return slog.Default()
}

type mockResolver struct {
	mu      sync.Mutex
	calls   []resolveCall
	err     error
	resolve chan struct{}
}

type resolveCall struct {
	ip        string
	clusterID uint16
}

func (m *mockResolver) ResolveIPCacheMiss(_ context.Context, ip net.IP, clusterID uint16) error {
	m.mu.Lock()
	m.calls = append(m.calls, resolveCall{ip: ip.String(), clusterID: clusterID})
	m.mu.Unlock()
	if m.resolve != nil {
		close(m.resolve)
	}
	return m.err
}

func (m *mockResolver) getCalls() []resolveCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	c := make([]resolveCall, len(m.calls))
	copy(c, m.calls)
	return c
}

func TestIPCacheMissHandler_HandleMiss(t *testing.T) {
	resolver := &mockResolver{resolve: make(chan struct{})}

	handler := &IPCacheMissHandler{
		logger:   testLogger(),
		resolver: resolver,
	}

	ctx := context.Background()

	// Send a miss signal
	data := IPCacheMissData{DstIP: 0x0a000001, ClusterID: 5}
	err := handler.handleMiss(ctx, data)
	require.NoError(t, err)

	// Wait for async resolution
	select {
	case <-resolver.resolve:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for resolution")
	}

	calls := resolver.getCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, "10.0.0.1", calls[0].ip)
	assert.Equal(t, uint16(5), calls[0].clusterID)
}

func TestIPCacheMissHandler_Deduplication(t *testing.T) {
	resolver := &mockResolver{}

	handler := &IPCacheMissHandler{
		logger:   nil,
		resolver: resolver,
	}

	ctx := context.Background()
	data := IPCacheMissData{DstIP: 0x0a000001, ClusterID: 5}

	// Pre-populate the inflight map to simulate an in-progress resolution
	handler.inflight.Store("10.0.0.1", struct{}{})

	// This should be deduplicated (no-op)
	err := handler.handleMiss(ctx, data)
	require.NoError(t, err)

	// Give a moment for any async work
	time.Sleep(50 * time.Millisecond)

	calls := resolver.getCalls()
	assert.Len(t, calls, 0, "should be deduplicated")
}
