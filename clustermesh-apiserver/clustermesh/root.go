// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package clustermesh

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path"
	"sync"

	"github.com/cilium/hive/cell"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"

	cmk8s "github.com/cilium/cilium/clustermesh-apiserver/clustermesh/k8s"
	"github.com/cilium/cilium/clustermesh-apiserver/syncstate"
	operatorWatchers "github.com/cilium/cilium/operator/watchers"
	"github.com/cilium/cilium/pkg/clustermesh/mcsapi"
	"github.com/cilium/cilium/pkg/clustermesh/operator"
	cmtypes "github.com/cilium/cilium/pkg/clustermesh/types"
	cmutils "github.com/cilium/cilium/pkg/clustermesh/utils"
	"github.com/cilium/cilium/pkg/hive"
	"github.com/cilium/cilium/pkg/identity"
	identityCache "github.com/cilium/cilium/pkg/identity/cache"
	"github.com/cilium/cilium/pkg/ipcache"
	ciliumv2 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2"
	k8sClient "github.com/cilium/cilium/pkg/k8s/client"
	"github.com/cilium/cilium/pkg/k8s/resource"
	"github.com/cilium/cilium/pkg/k8s/types"
	"github.com/cilium/cilium/pkg/kvstore"
	"github.com/cilium/cilium/pkg/kvstore/store"
	"github.com/cilium/cilium/pkg/labels"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/metrics"
	nodeStore "github.com/cilium/cilium/pkg/node/store"
	nodeTypes "github.com/cilium/cilium/pkg/node/types"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/promise"
	"github.com/cilium/cilium/pkg/version"
)

var (
	log = logging.DefaultLogger.WithField(logfields.LogSubsys, "clustermesh-apiserver")
)

const (
	// LabelSync is the label key used to select which CiliumEndpoints are
	// synced to the clustermesh kvstore. When sync-label filtering is enabled,
	// only endpoints whose owning Pod carries this label with value "true"
	// will be propagated.
	LabelSync = "clustermesh.cilium.io/sync"

	// AnnotationSyncScope is the annotation key that controls the visibility
	// scope of a synced endpoint. Its value (e.g. "global", "regional") is
	// propagated into the IPIdentityPair stored in the kvstore.
	AnnotationSyncScope = "clustermesh.cilium.io/sync-scope"
)

// EnableSyncLabelFilter controls whether the clustermesh-apiserver filters
// endpoints based on the LabelSync label. Defaults to false; can be enabled
// via CILIUM_ENABLE_SYNC_LABEL_FILTER=true environment variable.
var EnableSyncLabelFilter = false

func NewCmd(h *hive.Hive) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "clustermesh",
		Short: "Run ClusterMesh",
		Run: func(cmd *cobra.Command, args []string) {
			if err := h.Run(slog.Default()); err != nil {
				log.Fatal(err)
			}
		},
		PreRun: func(cmd *cobra.Command, args []string) {
			// Overwrite the metrics namespace with the one specific for the ClusterMesh API Server
			metrics.Namespace = metrics.CiliumClusterMeshAPIServerNamespace
			option.Config.SetupLogging(h.Viper(), "clustermesh-apiserver")
			option.Config.Populate(h.Viper())
			option.LogRegisteredOptions(h.Viper(), log)
			log.Infof("Cilium ClusterMesh %s", version.Version)
		},
	}

	h.RegisterFlags(rootCmd.Flags())
	rootCmd.AddCommand(h.Command())
	return rootCmd
}

type parameters struct {
	cell.In

	ExternalWorkloadsConfig
	CfgMCSAPI      operator.MCSAPIConfig
	ClusterInfo    cmtypes.ClusterInfo
	Clientset      k8sClient.Clientset
	Resources      cmk8s.Resources
	BackendPromise promise.Promise[kvstore.BackendOperations]
	StoreFactory   store.Factory
	SyncState      syncstate.SyncState

	Logger *slog.Logger
}

func registerHooks(lc cell.Lifecycle, params parameters) error {
	lc.Append(cell.Hook{
		OnStart: func(ctx cell.HookContext) error {
			if !params.Clientset.IsEnabled() {
				return errors.New("Kubernetes client not configured, cannot continue.")
			}

			backend, err := params.BackendPromise.Await(ctx)
			if err != nil {
				return err
			}

			startServer(ctx, params.ClusterInfo, params.EnableExternalWorkloads, params.Clientset, backend, params.Resources, params.StoreFactory, params.SyncState, params.CfgMCSAPI.ClusterMeshEnableMCSAPI, params.Logger)
			return nil
		},
	})
	return nil
}

type identitySynchronizer struct {
	store        store.SyncStore
	syncCallback func(context.Context)
}

func newIdentitySynchronizer(ctx context.Context, cinfo cmtypes.ClusterInfo, backend kvstore.BackendOperations, factory store.Factory, syncCallback func(context.Context)) synchronizer {
	identitiesStore := factory.NewSyncStore(cinfo.Name, backend,
		path.Join(identityCache.IdentitiesPath, "id"),
		store.WSSWithSyncedKeyOverride(identityCache.IdentitiesPath))
	go identitiesStore.Run(ctx)

	return &identitySynchronizer{store: identitiesStore, syncCallback: syncCallback}
}

func parseLabelArrayFromMap(base map[string]string) labels.LabelArray {
	array := make(labels.LabelArray, 0, len(base))
	for sourceAndKey, value := range base {
		array = append(array, labels.NewLabel(sourceAndKey, value, ""))
	}
	return array.Sort()
}

func (is *identitySynchronizer) upsert(ctx context.Context, _ resource.Key, obj runtime.Object) error {
	identity := obj.(*ciliumv2.CiliumIdentity)
	scopedLog := log.WithField(logfields.Identity, identity.Name)
	if len(identity.SecurityLabels) == 0 {
		scopedLog.WithError(errors.New("missing security labels")).Warning("Ignoring invalid identity")
		// Do not return an error, since it is pointless to retry.
		// We will receive a new update event if the security labels change.
		return nil
	}

	labelArray := parseLabelArrayFromMap(identity.SecurityLabels)

	var labels []byte
	for _, l := range labelArray {
		labels = append(labels, l.FormatForKVStore()...)
	}

	scopedLog.Info("Upserting identity in etcd")
	kv := store.NewKVPair(identity.Name, string(labels))
	if err := is.store.UpsertKey(ctx, kv); err != nil {
		// The only errors surfaced by WorkqueueSyncStore are the unrecoverable ones.
		log.WithError(err).Warning("Unable to upsert identity in etcd")
	}

	return nil
}

func (is *identitySynchronizer) delete(ctx context.Context, key resource.Key) error {
	scopedLog := log.WithField(logfields.Identity, key.Name)
	scopedLog.Info("Deleting identity from etcd")

	if err := is.store.DeleteKey(ctx, store.NewKVPair(key.Name, "")); err != nil {
		// The only errors surfaced by WorkqueueSyncStore are the unrecoverable ones.
		scopedLog.WithError(err).Warning("Unable to delete node from etcd")
	}

	return nil
}

func (is *identitySynchronizer) synced(ctx context.Context) error {
	log.Info("Initial list of identities successfully received from Kubernetes")
	return is.store.Synced(ctx, is.syncCallback)
}

type nodeStub struct {
	cluster string
	name    string
}

func (n *nodeStub) GetKeyName() string {
	return nodeTypes.GetKeyNodeName(n.cluster, n.name)
}

type nodeSynchronizer struct {
	clusterInfo  cmtypes.ClusterInfo
	store        store.SyncStore
	syncCallback func(context.Context)
}

func newNodeSynchronizer(ctx context.Context, cinfo cmtypes.ClusterInfo, backend kvstore.BackendOperations, factory store.Factory, syncCallback func(context.Context)) synchronizer {
	nodesStore := factory.NewSyncStore(cinfo.Name, backend, nodeStore.NodeStorePrefix)
	go nodesStore.Run(ctx)

	return &nodeSynchronizer{clusterInfo: cinfo, store: nodesStore, syncCallback: syncCallback}
}

func (ns *nodeSynchronizer) upsert(ctx context.Context, _ resource.Key, obj runtime.Object) error {
	n := nodeTypes.ParseCiliumNode(obj.(*ciliumv2.CiliumNode))
	n.Cluster = ns.clusterInfo.Name
	n.ClusterID = ns.clusterInfo.ID

	scopedLog := log.WithField(logfields.Node, n.Name)
	scopedLog.Info("Upserting node in etcd")

	if err := ns.store.UpsertKey(ctx, &n); err != nil {
		// The only errors surfaced by WorkqueueSyncStore are the unrecoverable ones.
		log.WithError(err).Warning("Unable to upsert node in etcd")
	}

	return nil
}

func (ns *nodeSynchronizer) delete(ctx context.Context, key resource.Key) error {
	n := nodeStub{
		cluster: ns.clusterInfo.Name,
		name:    key.Name,
	}

	scopedLog := log.WithFields(logrus.Fields{logfields.Node: key.Name})
	scopedLog.Info("Deleting node from etcd")

	if err := ns.store.DeleteKey(ctx, &n); err != nil {
		// The only errors surfaced by WorkqueueSyncStore are the unrecoverable ones.
		scopedLog.WithError(err).Warning("Unable to delete node from etcd")
	}

	return nil
}

func (ns *nodeSynchronizer) synced(ctx context.Context) error {
	log.Info("Initial list of nodes successfully received from Kubernetes")
	return ns.store.Synced(ctx, ns.syncCallback)
}

type ipmap map[string]struct{}

type endpointSynchronizer struct {
	store         store.SyncStore
	cache         map[string]ipmap
	syncCallback  func(context.Context)
	labelFiltered bool
}

func newEndpointSynchronizer(ctx context.Context, cinfo cmtypes.ClusterInfo, backend kvstore.BackendOperations, factory store.Factory, syncCallback func(context.Context), labelFiltered bool) synchronizer {
	endpointsStore := factory.NewSyncStore(cinfo.Name, backend,
		path.Join(ipcache.IPIdentitiesPath, ipcache.DefaultAddressSpace),
		store.WSSWithSyncedKeyOverride(ipcache.IPIdentitiesPath))
	go endpointsStore.Run(ctx)

	return &endpointSynchronizer{
		store:         endpointsStore,
		cache:         make(map[string]ipmap),
		syncCallback:  syncCallback,
		labelFiltered: labelFiltered,
	}
}

func (es *endpointSynchronizer) upsert(ctx context.Context, key resource.Key, obj runtime.Object) error {
	endpoint := obj.(*types.CiliumEndpoint)

	// If label filtering is enabled, only sync endpoints whose owning Pod
	// carries the clustermesh.cilium.io/sync="true" label. The label is
	// expected to be propagated to the CiliumEndpoint object by the agent.
	if es.labelFiltered {
		if endpoint.Labels == nil || endpoint.Labels[LabelSync] != "true" {
			// Endpoint should not be synced — delete any previously
			// synced IPs and return.
			return es.delete(ctx, key)
		}
	}

	ips := make(ipmap)
	stale := es.cache[key.String()]

	// Determine sync scope from annotation on the CiliumEndpoint.
	syncScope := ""
	if endpoint.Annotations != nil {
		syncScope = endpoint.Annotations[AnnotationSyncScope]
	}
	if syncScope != "" && syncScope != "regional" && syncScope != "global" {
		log.WithFields(logrus.Fields{
			logfields.Endpoint: key.String(),
			"syncScope":        syncScope,
		}).Warning("Unrecognized sync-scope annotation value, skipping endpoint propagation")
		return nil
	}

	if n := endpoint.Networking; n != nil {
		for _, address := range n.Addressing {
			for _, ip := range []string{address.IPV4, address.IPV6} {
				if ip == "" {
					continue
				}

				scopedLog := log.WithFields(logrus.Fields{logfields.Endpoint: key.String(), logfields.IPAddr: ip})
				entry := identity.IPIdentityPair{
					IP:           net.ParseIP(ip),
					HostIP:       net.ParseIP(n.NodeIP),
					K8sNamespace: endpoint.Namespace,
					K8sPodName:   endpoint.Name,
				}

				if endpoint.Identity != nil {
					entry.ID = identity.NumericIdentity(endpoint.Identity.ID)
				}

				if endpoint.Encryption != nil {
					entry.Key = uint8(endpoint.Encryption.Key)
				}

				// Propagate sync scope into the IPIdentityPair so that
				// downstream consumers (e.g. kvstoremesh regional filter)
				// can act on it.
				if syncScope != "" {
					entry.SyncScope = syncScope
				}

				scopedLog.Info("Upserting endpoint in etcd")
				if err := es.store.UpsertKey(ctx, &entry); err != nil {
					// The only errors surfaced by WorkqueueSyncStore are the unrecoverable ones.
					scopedLog.WithError(err).Warning("Unable to upsert endpoint in etcd")
					continue
				}

				ips[ip] = struct{}{}
				delete(stale, ip)
			}
		}
	}

	// Delete the stale endpoint IPs from the KVStore.
	es.deleteEndpoints(ctx, key, stale)
	es.cache[key.String()] = ips

	return nil
}

func (es *endpointSynchronizer) delete(ctx context.Context, key resource.Key) error {
	es.deleteEndpoints(ctx, key, es.cache[key.String()])
	delete(es.cache, key.String())
	return nil
}

func (es *endpointSynchronizer) synced(ctx context.Context) error {
	log.Info("Initial list of endpoints successfully received from Kubernetes")
	return es.store.Synced(ctx, es.syncCallback)
}

func (es *endpointSynchronizer) deleteEndpoints(ctx context.Context, key resource.Key, ips ipmap) {
	for ip := range ips {
		scopedLog := log.WithFields(logrus.Fields{logfields.Endpoint: key.String(), logfields.IPAddr: ip})
		scopedLog.Info("Deleting endpoint from etcd")

		entry := identity.IPIdentityPair{IP: net.ParseIP(ip)}
		if err := es.store.DeleteKey(ctx, &entry); err != nil {
			// The only errors surfaced by WorkqueueSyncStore are the unrecoverable ones.
			scopedLog.WithError(err).Warning("Unable to delete endpoint from etcd")
		}
	}
}

type synchronizer interface {
	upsert(ctx context.Context, key resource.Key, obj runtime.Object) error
	delete(ctx context.Context, key resource.Key) error
	synced(ctx context.Context) error
}

func synchronize[T runtime.Object](ctx context.Context, r resource.Resource[T], sync synchronizer) {
	for event := range r.Events(ctx) {
		switch event.Kind {
		case resource.Upsert:
			event.Done(sync.upsert(ctx, event.Key, event.Object))
		case resource.Delete:
			event.Done(sync.delete(ctx, event.Key))
		case resource.Sync:
			event.Done(sync.synced(ctx))
		}
	}
}

func startServer(
	startCtx cell.HookContext,
	cinfo cmtypes.ClusterInfo,
	allServices bool,
	clientset k8sClient.Clientset,
	backend kvstore.BackendOperations,
	resources cmk8s.Resources,
	factory store.Factory,
	syncState syncstate.SyncState,
	clusterMeshEnableMCSAPI bool,
	logger *slog.Logger,
) {
	log.WithFields(logrus.Fields{
		"cluster-name": cinfo.Name,
		"cluster-id":   cinfo.ID,
	}).Info("Starting clustermesh-apiserver...")

	config := cmtypes.CiliumClusterConfig{
		ID: cinfo.ID,
		Capabilities: cmtypes.CiliumClusterConfigCapabilities{
			SyncedCanaries:        true,
			MaxConnectedClusters:  cinfo.MaxConnectedClusters,
			ServiceExportsEnabled: &clusterMeshEnableMCSAPI,
		},
	}

	_, err := cmutils.EnforceClusterConfig(context.Background(), cinfo.Name, config, backend, log)
	if err != nil {
		log.WithError(err).Fatal("Unable to set local cluster config on kvstore")
	}

	// Check whether sync-label filtering is enabled via environment variable.
	labelFiltered := EnableSyncLabelFilter || os.Getenv("CILIUM_ENABLE_SYNC_LABEL_FILTER") == "true"
	if labelFiltered {
		log.Info("Sync-label filtering enabled: only CiliumEndpoints with clustermesh.cilium.io/sync=true will be synced")
	}

	ctx := context.Background()
	go synchronize(ctx, resources.CiliumIdentities, newIdentitySynchronizer(ctx, cinfo, backend, factory, syncState.WaitForResource()))
	go synchronize(ctx, resources.CiliumNodes, newNodeSynchronizer(ctx, cinfo, backend, factory, syncState.WaitForResource()))
	go synchronize(ctx, resources.CiliumSlimEndpoints, newEndpointSynchronizer(ctx, cinfo, backend, factory, syncState.WaitForResource(), labelFiltered))
	operatorWatchers.StartSynchronizingServices(ctx, &sync.WaitGroup{}, operatorWatchers.ServiceSyncParameters{
		ClusterInfo:  cinfo,
		Clientset:    clientset,
		Services:     resources.Services,
		Endpoints:    resources.Endpoints,
		Backend:      backend,
		SharedOnly:   !allServices,
		StoreFactory: factory,
		SyncCallback: syncState.WaitForResource(),
	}, logger)
	go mcsapi.StartSynchronizingServiceExports(ctx, mcsapi.ServiceExportSyncParameters{
		ClusterName:             cinfo.Name,
		ClusterMeshEnableMCSAPI: clusterMeshEnableMCSAPI,
		Clientset:               clientset,
		ServiceExports:          resources.ServiceExports,
		Services:                resources.Services,
		Backend:                 backend,
		StoreFactory:            factory,
		SyncCallback:            syncState.WaitForResource(),
	})
	syncState.Stop()

	log.Info("Initialization complete")
}
