// Copyright 2019 Altinity Ltd and/or its affiliates. All rights reserved.
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

package chi

import (
	"context"
	"fmt"
	"time"

	"github.com/juliangruber/go-intersect"
	"gopkg.in/d4l3k/messagediff.v1"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/util/workqueue"

	chop "github.com/altinity/clickhouse-operator/pkg/apis/clickhouse.altinity.com/v1"
	chopmodel "github.com/altinity/clickhouse-operator/pkg/model"
	"github.com/altinity/clickhouse-operator/pkg/util"
)

const FinalizerName = "finalizer.clickhouseinstallation.altinity.com"

// worker represents worker thread which runs reconcile tasks
type worker struct {
	c          *Controller
	a          Announcer
	queue      workqueue.RateLimitingInterface
	normalizer *chopmodel.Normalizer
	schemer    *chopmodel.Schemer
	creator    *chopmodel.Creator
	start      time.Time
}

// newWorker
func (c *Controller) newWorker(queue workqueue.RateLimitingInterface) *worker {
	return &worker{
		c:          c,
		a:          NewAnnouncer(c),
		queue:      queue,
		normalizer: chopmodel.NewNormalizer(c.chop),
		schemer: chopmodel.NewSchemer(
			c.chop.Config().CHUsername,
			c.chop.Config().CHPassword,
			c.chop.Config().CHPort,
		),
		creator: nil,
		start:   time.Now().Add(chop.DefaultReconcileThreadsWarmup),
	}
}

// run is an endless work loop, expected to be run in a thread
func (w *worker) run() {
	w.a.V(2).Info("run() - start")
	defer w.a.V(2).Info("run() - end")

	ctx := context.Background()
	util.WaitContextDoneUntil(ctx, w.start)

	for {
		// Get() blocks until it can return an item
		item, shutdown := w.queue.Get()
		if shutdown {
			w.a.Info("shutdown request")
			return
		}

		if err := w.processItem(ctx, item); err != nil {
			// Item not processed
			// this code cannot return an error and needs to indicate error has been ignored
			utilruntime.HandleError(err)
		}

		// Forget indicates that an item is finished being retried.  Doesn't matter whether its for perm failing
		// or for success, we'll stop the rate limiter from tracking it.  This only clears the `rateLimiter`, you
		// still have to call `Done` on the queue.
		w.queue.Forget(item)

		// Remove item from processing set when processing completed
		w.queue.Done(item)
	}
}

// processItem processes one work item according to its type
func (w *worker) processItem(ctx context.Context, item interface{}) error {
	w.a.V(3).Info("processItem() - start")
	defer w.a.V(3).Info("processItem() - end")

	if util.IsContextDone(ctx) {
		return nil
	}

	switch command := item.(type) {

	case *ReconcileCHI:
		switch command.cmd {
		case reconcileAdd:
			return w.updateCHI(ctx, nil, command.new)
		case reconcileUpdate:
			return w.updateCHI(ctx, command.old, command.new)
		case reconcileDelete:
			return w.deleteCHI(ctx, command.old)
		}

		// Unknown item type, don't know what to do with it
		// Just skip it and behave like it never existed
		utilruntime.HandleError(fmt.Errorf("unexpected reconcile - %#v", command))
		return nil

	case *ReconcileCHIT:
		switch command.cmd {
		case reconcileAdd:
			return w.c.addChit(command.new)
		case reconcileUpdate:
			return w.c.updateChit(command.old, command.new)
		case reconcileDelete:
			return w.c.deleteChit(command.old)
		}

		// Unknown item type, don't know what to do with it
		// Just skip it and behave like it never existed
		utilruntime.HandleError(fmt.Errorf("unexpected reconcile - %#v", command))
		return nil

	case *ReconcileChopConfig:
		switch command.cmd {
		case reconcileAdd:
			return w.c.addChopConfig(command.new)
		case reconcileUpdate:
			return w.c.updateChopConfig(command.old, command.new)
		case reconcileDelete:
			return w.c.deleteChopConfig(command.old)
		}

		// Unknown item type, don't know what to do with it
		// Just skip it and behave like it never existed
		utilruntime.HandleError(fmt.Errorf("unexpected reconcile - %#v", command))
		return nil

	case *DropDns:
		if chi, err := w.createCHIFromObjectMeta(command.initiator); err == nil {
			w.a.V(2).Info("endpointsInformer UpdateFunc(%s/%s) flushing DNS for CHI %s", command.initiator.Namespace, command.initiator.Name, chi.Name)
			_ = w.schemer.CHIDropDnsCache(ctx, chi)
		} else {
			w.a.Error("endpointsInformer UpdateFunc(%s/%s) unable to find CHI by %v", command.initiator.Namespace, command.initiator.Name, command.initiator.Labels)
		}
		return nil
	}

	// Unknown item type, don't know what to do with it
	// Just skip it and behave like it never existed
	utilruntime.HandleError(fmt.Errorf("unexpected item in the queue - %#v", item))
	return nil
}

// normalize
func (w *worker) normalize(chi *chop.ClickHouseInstallation) *chop.ClickHouseInstallation {
	w.a.V(3).Info("normalize() - start")
	defer w.a.V(3).Info("normalize() - end")

	var withDefaultCluster bool

	if chi == nil {
		chi = &chop.ClickHouseInstallation{}
		withDefaultCluster = false
	} else {
		withDefaultCluster = true
	}

	chi, err := w.normalizer.CreateTemplatedCHI(chi, withDefaultCluster)
	if err != nil {
		w.a.WithEvent(chi, eventActionReconcile, eventReasonReconcileFailed).
			WithStatusError(chi).
			Error("FAILED to normalize CHI : %v", err)
	}

	return chi
}

// ensureFinalizer
func (w *worker) ensureFinalizer(chi *chop.ClickHouseInstallation) {
	namespace, name := util.NamespaceName(chi.ObjectMeta)

	// Check whether finalizer is already listed in CHI
	if util.InArray(FinalizerName, chi.ObjectMeta.Finalizers) {
		w.a.V(2).Info("ensureFinalizer(%s/%s): finalizer already installed", namespace, name)
	}

	// No finalizer found - need to install it

	if err := w.c.installFinalizer(chi); err != nil {
		w.a.V(1).Info("ensureFinalizer(%s/%s): unable to install finalizer. err: %v", namespace, name, err)
	}

	w.a.V(3).Info("ensureFinalizer(%s/%s): finalizer installed", namespace, name)
}

// updateCHI sync CHI which was already created earlier
func (w *worker) updateCHI(ctx context.Context, old, new *chop.ClickHouseInstallation) error {
	w.a.V(3).Info("updateCHI() - start")
	defer w.a.V(3).Info("updateCHI() - end")

	update := (old != nil) && (new != nil)

	if update && (old.ObjectMeta.ResourceVersion == new.ObjectMeta.ResourceVersion) {
		w.a.V(3).Info("updateCHI(%s/%s): ResourceVersion did not change: %s", new.Namespace, new.Name, new.ObjectMeta.ResourceVersion)
		// No need to react
		return nil
	}

	// Check DeletionTimestamp in order to understand, whether the object is being deleted
	if new.ObjectMeta.DeletionTimestamp.IsZero() {
		// The object is not being deleted
		w.ensureFinalizer(new)
	} else {
		// The object is being deleted
		return w.finalizeCHI(ctx, new)
	}

	old = w.normalize(old)
	new = w.normalize(new)

	actionPlan := NewActionPlan(old, new)

	if !actionPlan.HasActionsToDo() {
		// Nothing to do - no changes found - no need to react
		w.a.V(3).Info("updateCHI(%s/%s) - ResourceVersion changed, but no actual changes found", new.Namespace, new.Name)
		return nil
	}

	// Write desired normalized CHI with initialized .Status, so it would be possible to monitor progress
	(&new.Status).ReconcileStart(actionPlan.GetRemovedHostsNum())
	if err := w.c.updateCHIObjectStatus(ctx, new, false); err != nil {
		w.a.V(1).Info("UNABLE to write normalized CHI (%s/%s). It can trigger update action again. Error: %q", new.Namespace, new.Name, err)
		return nil
	}

	w.a.V(1).
		WithEvent(new, eventActionReconcile, eventReasonReconcileStarted).
		WithStatusAction(new).
		Info("updateCHI(%s/%s) reconcile started", new.Namespace, new.Name)
	w.a.V(2).Info("updateCHI(%s/%s) - action plan\n%s\n", new.Namespace, new.Name, actionPlan.String())

	if new.IsStopped() {
		w.a.V(1).
			WithEvent(new, eventActionReconcile, eventReasonReconcileInProgress).
			WithStatusAction(new).
			Info("updateCHI(%s/%s) exclude CHI from monitoring", new.Namespace, new.Name)
		w.c.deleteWatch(new.Namespace, new.Name)
	}

	actionPlan.WalkAdded(
		func(cluster *chop.ChiCluster) {
			cluster.WalkHosts(func(host *chop.ChiHost) error {
				(&host.ReconcileAttributes).SetAdd()
				return nil
			})
		},
		func(shard *chop.ChiShard) {
			shard.WalkHosts(func(host *chop.ChiHost) error {
				(&host.ReconcileAttributes).SetAdd()
				return nil
			})
		},
		func(host *chop.ChiHost) {
			(&host.ReconcileAttributes).SetAdd()
		},
	)

	actionPlan.WalkModified(
		func(cluster *chop.ChiCluster) {
		},
		func(shard *chop.ChiShard) {
		},
		func(host *chop.ChiHost) {
			(&host.ReconcileAttributes).SetModify()
		},
	)

	new.WalkHosts(func(host *chop.ChiHost) error {
		if update {
			host.ReconcileAttributes.SetMigrate()
		}
		return nil
	})

	new.WalkHosts(func(host *chop.ChiHost) error {
		if host.ReconcileAttributes.IsAdd() {
			// Already added
		} else if host.ReconcileAttributes.IsModify() {
			// Already modified
		} else {
			// Not clear yet
			(&host.ReconcileAttributes).SetUnclear()
		}
		return nil
	})

	new.WalkHosts(func(host *chop.ChiHost) error {
		if host.ReconcileAttributes.IsAdd() {
			w.a.Info("ADD host: %s", host.Address.ShortString())
		} else if host.ReconcileAttributes.IsModify() {
			w.a.Info("MODIFY host: %s", host.Address.ShortString())
		} else if host.ReconcileAttributes.IsUnclear() {
			w.a.Info("UNCLEAR host: %s", host.Address.ShortString())
		} else {
			w.a.Info("UNTOUCH host: %s", host.Address.ShortString())
		}
		return nil
	})

	if err := w.reconcile(ctx, new); err != nil {
		w.a.WithEvent(new, eventActionReconcile, eventReasonReconcileFailed).
			WithStatusError(new).
			Error("FAILED update: %v", err)
		return nil
	}

	// Post-process added items
	w.a.V(1).
		WithEvent(new, eventActionReconcile, eventReasonReconcileInProgress).
		WithStatusAction(new).
		Info("updateCHI(%s/%s) remove scheduled for deletion items", new.Namespace, new.Name)
	actionPlan.WalkAdded(
		func(cluster *chop.ChiCluster) {
		},
		func(shard *chop.ChiShard) {
		},
		func(host *chop.ChiHost) {
			if update {
				w.a.V(1).
					WithEvent(new, eventActionCreate, eventReasonCreateStarted).
					WithStatusAction(new).
					Info("Adding tables on shard/host:%d/%d cluster:%s", host.Address.ShardIndex, host.Address.ReplicaIndex, host.Address.ClusterName)
				if err := w.schemer.HostCreateTables(ctx, host); err != nil {
					w.a.Error("ERROR create tables on host %s. err: %v", host.Name, err)
				}
			} else {
				w.a.V(1).
					Info("As CHI is just created, not need to add tables on host %d to shard %d in cluster %s", host.Address.ReplicaIndex, host.Address.ShardIndex, host.Address.ClusterName)
			}
		},
	)

	// Remove deleted items
	w.a.V(1).
		WithEvent(new, eventActionReconcile, eventReasonReconcileInProgress).
		WithStatusAction(new).
		Info("updateCHI(%s/%s) remove scheduled for deletion items", new.Namespace, new.Name)
	actionPlan.WalkRemoved(
		func(cluster *chop.ChiCluster) {
			_ = w.deleteCluster(ctx, cluster)
		},
		func(shard *chop.ChiShard) {
			_ = w.deleteShard(ctx, shard)
		},
		func(host *chop.ChiHost) {
			_ = w.deleteHost(ctx, host)
		},
	)

	if !new.IsStopped() {
		w.a.V(1).
			WithEvent(new, eventActionReconcile, eventReasonReconcileInProgress).
			WithStatusAction(new).
			Info("updateCHI(%s/%s) add CHI to monitoring", new.Namespace, new.Name)
		w.c.updateWatch(new.Namespace, new.Name, chopmodel.CreatePodFQDNsOfCHI(new))
	}

	// Update CHI object
	(&new.Status).ReconcileComplete()
	_ = w.c.updateCHIObjectStatus(ctx, new, false)

	w.a.V(1).
		WithEvent(new, eventActionReconcile, eventReasonReconcileCompleted).
		WithStatusActions(new).
		Info("updateCHI(%s/%s) reconcile completed", new.Namespace, new.Name)

	return nil
}

// reconcile reconciles ClickHouseInstallation
func (w *worker) reconcile(ctx context.Context, chi *chop.ClickHouseInstallation) error {
	w.a.V(2).Info("reconcile() - start")
	defer w.a.V(2).Info("reconcile() - end")

	w.creator = chopmodel.NewCreator(w.c.chop, chi)
	return chi.WalkTillError(
		ctx,
		w.reconcileCHIAuxObjectsPreliminary,
		w.reconcileCluster,
		w.reconcileShard,
		w.reconcileHost,
		w.reconcileCHIAuxObjectsFinal,
	)
}

// reconcileCHIAuxObjectsPreliminary reconciles CHI preliminary in order to ensure that ConfigMaps are in place
func (w *worker) reconcileCHIAuxObjectsPreliminary(ctx context.Context, chi *chop.ClickHouseInstallation) error {
	w.a.V(2).Info("reconcileCHIAuxObjectsPreliminary() - start")
	defer w.a.V(2).Info("reconcileCHIAuxObjectsPreliminary() - end")

	if util.IsContextDone(ctx) {
		return nil
	}

	// 1. CHI Service
	if chi.IsStopped() {
		// Stopped cluster must have no entry point
		_ = w.c.deleteServiceCHI(ctx, chi)
	} else {
		service := w.creator.CreateServiceCHI()
		if err := w.reconcileService(ctx, chi, service); err != nil {
			return err
		}
	}

	// 2. CHI ConfigMaps without update - create only
	return w.reconcileCHIConfigMaps(ctx, chi, nil, false)
}

// reconcileCHIAuxObjectsFinal reconciles CHI global objects
func (w *worker) reconcileCHIAuxObjectsFinal(ctx context.Context, chi *chop.ClickHouseInstallation) error {
	w.a.V(2).Info("reconcileCHIAuxObjectsFinal() - start")
	defer w.a.V(2).Info("reconcileCHIAuxObjectsFinal() - end")

	if util.IsContextDone(ctx) {
		return nil
	}

	// CHI ConfigMaps with update
	return w.reconcileCHIConfigMaps(ctx, chi, nil, true)
}

// reconcileCHIConfigMaps reconciles all CHI's ConfigMaps
func (w *worker) reconcileCHIConfigMaps(
	ctx context.Context,
	chi *chop.ClickHouseInstallation,
	options *chopmodel.ClickHouseConfigFilesGeneratorOptions,
	update bool,
) error {
	if util.IsContextDone(ctx) {
		return nil
	}
	// ConfigMap common for all resources in CHI
	// contains several sections, mapped as separated chopConfig files,
	// such as remote servers, zookeeper setup, etc
	configMapCommon := w.creator.CreateConfigMapCHICommon(options)
	if err := w.reconcileConfigMap(ctx, chi, configMapCommon, update); err != nil {
		return err
	}

	// ConfigMap common for all users resources in CHI
	configMapUsers := w.creator.CreateConfigMapCHICommonUsers()
	if err := w.reconcileConfigMap(ctx, chi, configMapUsers, update); err != nil {
		return err
	}

	return nil
}

// reconcileCluster reconciles Cluster, excluding nested shards
func (w *worker) reconcileCluster(ctx context.Context, cluster *chop.ChiCluster) error {
	w.a.V(2).Info("reconcileCluster() - start")
	defer w.a.V(2).Info("reconcileCluster() - end")

	if util.IsContextDone(ctx) {
		return nil
	}

	// Add Cluster's Service
	service := w.creator.CreateServiceCluster(cluster)
	if service == nil {
		// TODO
		// For somewhat reason Service is not created, this is an error, but not clear what to do about it
		return nil
	}
	return w.reconcileService(ctx, cluster.CHI, service)
}

// reconcileShard reconciles Shard, excluding nested replicas
func (w *worker) reconcileShard(ctx context.Context, shard *chop.ChiShard) error {
	w.a.V(2).Info("reconcileShard() - start")
	defer w.a.V(2).Info("reconcileShard() - end")

	if util.IsContextDone(ctx) {
		return nil
	}

	// Add Shard's Service
	service := w.creator.CreateServiceShard(shard)
	if service == nil {
		// TODO
		// For somewhat reason Service is not created, this is an error, but not clear what to do about it
		return nil
	}
	return w.reconcileService(ctx, shard.CHI, service)
}

// reconcileHost reconciles ClickHouse host
func (w *worker) reconcileHost(ctx context.Context, host *chop.ChiHost) error {
	w.a.V(2).Info("reconcileHost() - start")
	defer w.a.V(2).Info("reconcileHost() - end")

	if util.IsContextDone(ctx) {
		return nil
	}

	w.a.V(1).
		WithEvent(host.CHI, eventActionReconcile, eventReasonReconcileStarted).
		WithStatusAction(host.CHI).
		Info("Reconcile Host %s started", host.Name)

	// Create artifacts
	configMap := w.creator.CreateConfigMapHost(host)
	statefulSet := w.creator.CreateStatefulSet(host)
	service := w.creator.CreateServiceHost(host)
	status := w.getStatefulSetStatus(host.StatefulSet)

	if err := w.excludeHost(ctx, host, status); err != nil {
		return err
	}

	// Reconcile host's ConfigMap
	if err := w.reconcileConfigMap(ctx, host.CHI, configMap, true); err != nil {
		return err
	}

	// Reconcile host's StatefulSet
	if err := w.reconcileStatefulSet(ctx, statefulSet, host); err != nil {
		return err
	}

	// Reconcile host's Persistent Volumes
	w.reconcilePersistentVolumes(ctx, host)

	// Reconcile host's Service
	if err := w.reconcileService(ctx, host.CHI, service); err != nil {
		return err
	}

	host.ReconcileAttributes.UnsetAdd()

	if host.ReconcileAttributes.IsMigrate() {
		w.a.V(1).
			WithEvent(host.CHI, eventActionCreate, eventReasonCreateStarted).
			WithStatusAction(host.CHI).
			Info("Adding tables on shard/host:%d/%d cluster:%s", host.Address.ShardIndex, host.Address.ReplicaIndex, host.Address.ClusterName)
		if err := w.schemer.HostCreateTables(ctx, host); err != nil {
			w.a.Error("ERROR create tables on host %s. err: %v", host.Name, err)
		}
	} else {
		w.a.V(1).
			Info("As CHI is just created, not need to add tables on host %d to shard %d in cluster %s", host.Address.ReplicaIndex, host.Address.ShardIndex, host.Address.ClusterName)
	}

	if err := w.includeHost(ctx, host, status); err != nil {
		// If host is not ready - fallback
		return err
	}

	host.ReconcileAttributes.SetReconciled()

	w.a.V(1).
		WithEvent(host.CHI, eventActionReconcile, eventReasonReconcileCompleted).
		WithStatusAction(host.CHI).
		Info("Reconcile Host %s completed", host.Name)

	return nil
}

// Exclude host from ClickHouse clusters
func (w *worker) excludeHost(ctx context.Context, host *chop.ChiHost, status StatefulSetStatus) error {
	if util.IsContextDone(ctx) {
		return nil
	}

	if w.shouldWaitExcludeHost(host, status) {
		w.a.V(1).
			Info("Exclude from cluster host %d shard %d cluster %s", host.Address.ReplicaIndex, host.Address.ShardIndex, host.Address.ClusterName)

		options := chopmodel.NewClickHouseConfigFilesGeneratorOptions().
			SetRemoteServersGeneratorOptions(chopmodel.NewRemoteServersGeneratorOptions().
				ExcludeHost(host).
				ExcludeReconcileAttributes(
					chop.NewChiHostReconcileAttributes().SetAdd(),
				),
			)

		_ = w.reconcileCHIConfigMaps(ctx, host.CHI, options, true) // remove host from cluster config only if we are going to wait for exclusion
		_ = w.waitHostNotInCluster(ctx, host)
	}

	return nil
}

// determines whether reconciler should wait for host to be excluded from/included into cluster
func (w *worker) shouldWaitExcludeHost(host *chop.ChiHost, status StatefulSetStatus) bool {

	if (status == statefulSetStatusNew) || (status == statefulSetStatusSame) {
		// No need to wait for new and non-modified StatefulSets
		return false
	}

	if host.GetShard().HostsCount() == 1 {
		// In case shard where current host is located has only one host (means no replication), no need to wait
		return false
	}

	if host.CHI.IsReconcilingPolicyWait() {
		return true
	} else if host.CHI.IsReconcilingPolicyNoWait() {
		return false
	}

	if !w.c.chop.Config().ReconcileWaitExclude {
		return false
	}

	return true
}

// determines whether reconciler should wait for host to be excluded from/included into cluster
func (w *worker) shouldWaitIncludeHost(host *chop.ChiHost, status StatefulSetStatus) bool {
	if (status == statefulSetStatusNew) || (status == statefulSetStatusSame) {
		return false
	}

	if host.GetShard().HostsCount() == 1 {
		// In case shard where current host is located has only one host (means no replication), no need to wait
		return false
	}

	if host.CHI.IsReconcilingPolicyWait() {
		return true
	} else if host.CHI.IsReconcilingPolicyNoWait() {
		return false
	}

	if w.c.chop.Config().ReconcileWaitInclude == false {
		return false
	}

	return true
}

// Include host back to ClickHouse clusters
func (w *worker) includeHost(ctx context.Context, host *chop.ChiHost, status StatefulSetStatus) error {
	w.a.V(1).
		Info("Include into cluster host %d shard %d cluster %s", host.Address.ReplicaIndex, host.Address.ShardIndex, host.Address.ClusterName)
	options := chopmodel.NewClickHouseConfigFilesGeneratorOptions().
		SetRemoteServersGeneratorOptions(chopmodel.NewRemoteServersGeneratorOptions().
			ExcludeReconcileAttributes(
				chop.NewChiHostReconcileAttributes().SetAdd(),
			),
		)
	_ = w.reconcileCHIConfigMaps(ctx, host.CHI, options, true)
	if w.shouldWaitIncludeHost(host, status) {
		_ = w.waitHostInCluster(ctx, host)
	}

	return nil
}

func (w *worker) waitHostInCluster(ctx context.Context, host *chop.ChiHost) error {
	return w.c.pollHostContext(ctx, host, nil, w.schemer.IsHostInCluster)
}

func (w *worker) waitHostNotInCluster(ctx context.Context, host *chop.ChiHost) error {
	return w.c.pollHostContext(ctx, host, nil, func(ctx context.Context, host *chop.ChiHost) bool {
		return !w.schemer.IsHostInCluster(ctx, host)
	})
}

// finalizeCHI
func (w *worker) finalizeCHI(ctx context.Context, chi *chop.ClickHouseInstallation) error {
	namespace, name := util.NamespaceName(chi.ObjectMeta)
	w.a.V(3).Info("finalizeCHI(%s/%s) - start", namespace, name)
	defer w.a.V(3).Info("finalizeCHI(%s/%s) - end", namespace, name)

	if util.IsContextDone(ctx) {
		return nil
	}

	cur, err := w.c.chopClient.ClickhouseV1().ClickHouseInstallations(namespace).Get(name, newGetOptions())
	if (err != nil) || (cur == nil) {
		return nil
	}

	if !util.InArray(FinalizerName, chi.ObjectMeta.Finalizers) {
		// No finalizer found, unexpected behavior
		return nil
	}

	// Delete CHI
	(&chi.Status).DeleteStart()
	if err := w.c.updateCHIObjectStatus(ctx, chi, true); err != nil {
		w.a.V(1).Info("UNABLE to write normalized CHI (%s/%s). err:%q", namespace, name, err)
		return nil
	}

	_ = w.deleteCHI(ctx, chi)

	// Uninstall finalizer
	w.a.V(2).Info("finalizeCHI(%s/%s): uninstall finalizer", namespace, name)
	if err := w.c.uninstallFinalizer(chi); err != nil {
		w.a.V(1).Info("finalizeCHI(%s/%s): unable to uninstall finalizer: err:%v", namespace, name, err)
	}

	return nil
}

// deleteCHI deletes all kubernetes resources related to chi *chop.ClickHouseInstallation
func (w *worker) deleteCHI(ctx context.Context, chi *chop.ClickHouseInstallation) error {
	w.a.V(2).Info("deleteCHI() - start")
	defer w.a.V(2).Info("deleteCHI() - end")

	if util.IsContextDone(ctx) {
		return nil
	}

	var err error

	w.a.V(1).
		WithEvent(chi, eventActionDelete, eventReasonDeleteStarted).
		WithStatusAction(chi).
		Info("Delete CHI %s/%s started", chi.Namespace, chi.Name)

	chi, err = w.normalizer.CreateTemplatedCHI(chi, true)
	if err != nil {
		w.a.WithEvent(chi, eventActionDelete, eventReasonDeleteFailed).
			WithStatusError(chi).
			Error("Delete CHI %s/%s failed - unable to normalize: %q", chi.Namespace, chi.Name, err)
		return err
	}

	// Exclude this CHI from monitoring
	w.c.deleteWatch(chi.Namespace, chi.Name)

	// Delete all clusters
	chi.WalkClusters(func(cluster *chop.ChiCluster) error {
		return w.deleteCluster(ctx, cluster)
	})

	if util.IsContextDone(ctx) {
		return nil
	}

	// Delete ConfigMap(s)
	err = w.c.deleteConfigMapsCHI(ctx, chi)

	// Delete Service
	err = w.c.deleteServiceCHI(ctx, chi)

	w.a.V(1).
		WithEvent(chi, eventActionDelete, eventReasonDeleteCompleted).
		WithStatusAction(chi).
		Info("Delete CHI %s/%s - completed", chi.Namespace, chi.Name)

	return nil
}

// deleteTables
func (w *worker) deleteTables(ctx context.Context, host *chop.ChiHost) error {
	if util.IsContextDone(ctx) {
		return nil
	}

	if !host.CanDeleteAllPVCs() {
		return nil
	}
	err := w.schemer.HostDeleteTables(ctx, host)

	if err == nil {
		w.a.V(1).
			WithEvent(host.CHI, eventActionDelete, eventReasonDeleteCompleted).
			WithStatusAction(host.CHI).
			Info("Deleted tables on host %s replica %d to shard %d in cluster %s",
				host.Name, host.Address.ReplicaIndex, host.Address.ShardIndex, host.Address.ClusterName)
	} else {
		w.a.WithEvent(host.CHI, eventActionDelete, eventReasonDeleteFailed).
			WithStatusError(host.CHI).
			Error("FAILED to delete tables on host %s with error %v", host.Name, err)
	}

	return err
}

// deleteHost deletes all kubernetes resources related to replica *chop.ChiHost
func (w *worker) deleteHost(ctx context.Context, host *chop.ChiHost) error {
	w.a.V(2).Info("deleteHost() - start")
	defer w.a.V(2).Info("deleteHost() - end")

	if util.IsContextDone(ctx) {
		return nil
	}

	w.a.V(1).
		WithEvent(host.CHI, eventActionDelete, eventReasonDeleteStarted).
		WithStatusAction(host.CHI).
		Info("Delete host %s/%s - started", host.Address.ClusterName, host.Name)

	if _, err := w.c.getStatefulSetByHost(host); err != nil {
		w.a.WithEvent(host.CHI, eventActionDelete, eventReasonDeleteCompleted).
			WithStatusAction(host.CHI).
			Info("Delete host %s/%s - completed StatefulSet not found - already deleted? err: %v",
				host.Address.ClusterName, host.Name, err)
		return nil
	}

	// Each host consists of
	// 1. User-level objects - tables on the host
	//    We need to delete tables on the host in order to clean Zookeeper data.
	//    If just delete tables, Zookeeper will still keep track of non-existent tables
	// 2. Kubernetes-level objects - such as StatefulSet, PVC(s), ConfigMap(s), Service(s)
	// Need to delete all these items

	var err error
	err = w.deleteTables(ctx, host)
	err = w.c.deleteHost(ctx, host)

	// When deleting the whole CHI (not particular host), CHI may already be unavailable, so update CHI tolerantly
	host.CHI.Status.DeletedHostsCount++
	_ = w.c.updateCHIObjectStatus(ctx, host.CHI, true)

	if err == nil {
		w.a.V(1).
			WithEvent(host.CHI, eventActionDelete, eventReasonDeleteCompleted).
			WithStatusAction(host.CHI).
			Info("Delete host %s/%s - completed", host.Address.ClusterName, host.Name)
	} else {
		w.a.WithEvent(host.CHI, eventActionDelete, eventReasonDeleteFailed).
			WithStatusError(host.CHI).
			Error("FAILED Delete host %s/%s - completed", host.Address.ClusterName, host.Name)
	}

	return err
}

// deleteShard deletes all kubernetes resources related to shard *chop.ChiShard
func (w *worker) deleteShard(ctx context.Context, shard *chop.ChiShard) error {
	w.a.V(2).Info("deleteShard() - start")
	defer w.a.V(2).Info("deleteShard() - end")

	if util.IsContextDone(ctx) {
		return nil
	}

	w.a.V(1).
		WithEvent(shard.CHI, eventActionDelete, eventReasonDeleteStarted).
		WithStatusAction(shard.CHI).
		Info("Delete shard %s/%s - started", shard.Address.Namespace, shard.Name)

	// Delete all replicas
	shard.WalkHosts(func(host *chop.ChiHost) error {
		return w.deleteHost(ctx, host)
	})

	// Delete Shard Service
	_ = w.c.deleteServiceShard(ctx, shard)

	w.a.V(1).
		WithEvent(shard.CHI, eventActionDelete, eventReasonDeleteCompleted).
		WithStatusAction(shard.CHI).
		Info("Delete shard %s/%s - completed", shard.Address.Namespace, shard.Name)

	return nil
}

// deleteCluster deletes all kubernetes resources related to cluster *chop.ChiCluster
func (w *worker) deleteCluster(ctx context.Context, cluster *chop.ChiCluster) error {
	w.a.V(2).Info("deleteCluster() - start")
	defer w.a.V(2).Info("deleteCluster() - end")

	if util.IsContextDone(ctx) {
		return nil
	}

	w.a.V(1).
		WithEvent(cluster.CHI, eventActionDelete, eventReasonDeleteStarted).
		WithStatusAction(cluster.CHI).
		Info("Delete cluster %s/%s - started", cluster.Address.Namespace, cluster.Name)

	// Delete all shards
	cluster.WalkShards(func(index int, shard *chop.ChiShard) error {
		return w.deleteShard(ctx, shard)
	})

	// Delete Cluster Service
	_ = w.c.deleteServiceCluster(ctx, cluster)

	w.a.V(1).
		WithEvent(cluster.CHI, eventActionDelete, eventReasonDeleteCompleted).
		WithStatusAction(cluster.CHI).
		Info("Delete cluster %s/%s - completed", cluster.Address.Namespace, cluster.Name)

	return nil
}

// createCHIFromObjectMeta
func (w *worker) createCHIFromObjectMeta(objectMeta *meta.ObjectMeta) (*chop.ClickHouseInstallation, error) {
	w.a.V(3).Info("createCHIFromObjectMeta() - start")
	defer w.a.V(3).Info("createCHIFromObjectMeta() - end")

	chi, err := w.c.GetCHIByObjectMeta(objectMeta)
	if err != nil {
		return nil, err
	}

	chi, err = w.normalizer.NormalizeCHI(chi)
	if err != nil {
		return nil, err
	}

	return chi, nil
}

// createClusterFromObjectMeta
func (w *worker) createClusterFromObjectMeta(objectMeta *meta.ObjectMeta) (*chop.ChiCluster, error) {
	w.a.V(3).Info("createClusterFromObjectMeta() - start")
	defer w.a.V(3).Info("createClusterFromObjectMeta() - end")

	clusterName, err := chopmodel.GetClusterNameFromObjectMeta(objectMeta)
	if err != nil {
		return nil, fmt.Errorf("ObjectMeta %s does not generated by CHI %v", objectMeta.Name, err)
	}

	chi, err := w.createCHIFromObjectMeta(objectMeta)
	if err != nil {
		return nil, err
	}

	cluster := chi.FindCluster(clusterName)
	if cluster == nil {
		return nil, fmt.Errorf("can't find cluster %s in CHI %s", clusterName, chi.Name)
	}

	return cluster, nil
}

// updateConfigMap
func (w *worker) updateConfigMap(ctx context.Context, chi *chop.ClickHouseInstallation, configMap *core.ConfigMap) error {
	if util.IsContextDone(ctx) {
		return nil
	}

	_, err := w.c.kubeClient.CoreV1().ConfigMaps(configMap.Namespace).Update(configMap)

	if err == nil {
		w.a.V(1).
			WithEvent(chi, eventActionUpdate, eventReasonUpdateCompleted).
			WithStatusAction(chi).
			Info("Update ConfigMap %s/%s", configMap.Namespace, configMap.Name)
	} else {
		w.a.WithEvent(chi, eventActionUpdate, eventReasonUpdateFailed).
			WithStatusAction(chi).
			WithStatusError(chi).
			Error("Update ConfigMap %s/%s failed with error %v", configMap.Namespace, configMap.Name, err)
	}

	return err
}

// createConfigMap
func (w *worker) createConfigMap(ctx context.Context, chi *chop.ClickHouseInstallation, configMap *core.ConfigMap) error {
	if util.IsContextDone(ctx) {
		return nil
	}

	_, err := w.c.kubeClient.CoreV1().ConfigMaps(configMap.Namespace).Create(configMap)

	if err == nil {
		w.a.V(1).
			WithEvent(chi, eventActionCreate, eventReasonCreateCompleted).
			WithStatusAction(chi).
			Info("Create ConfigMap %s/%s", configMap.Namespace, configMap.Name)
	} else {
		w.a.WithEvent(chi, eventActionCreate, eventReasonCreateFailed).
			WithStatusAction(chi).
			WithStatusError(chi).
			Error("Create ConfigMap %s/%s failed with error %v", configMap.Namespace, configMap.Name, err)
	}

	return err
}

// reconcileConfigMap reconciles core.ConfigMap which belongs to specified CHI
func (w *worker) reconcileConfigMap(
	ctx context.Context,
	chi *chop.ClickHouseInstallation,
	configMap *core.ConfigMap,
	update bool,
) error {
	w.a.V(2).Info("reconcileConfigMap() - start")
	defer w.a.V(2).Info("reconcileConfigMap() - end")

	if util.IsContextDone(ctx) {
		return nil
	}

	// Check whether this object already exists in k8s
	curConfigMap, err := w.c.getConfigMap(&configMap.ObjectMeta, false)

	if curConfigMap != nil {
		// We have ConfigMap - try to update it
		if !update {
			return nil
		}
		err = w.updateConfigMap(ctx, chi, configMap)
	}

	if apierrors.IsNotFound(err) {
		// ConfigMap not found - even during Update process - try to create it
		err = w.createConfigMap(ctx, chi, configMap)
	}

	if err != nil {
		w.a.WithEvent(chi, eventActionReconcile, eventReasonReconcileFailed).
			WithStatusAction(chi).
			WithStatusError(chi).
			Error("FAILED to reconcile ConfigMap: %s CHI: %s ", configMap.Name, chi.Name)
	}

	return err
}

// updateService
func (w *worker) updateService(
	ctx context.Context,
	chi *chop.ClickHouseInstallation,
	curService *core.Service,
	newService *core.Service,
) error {
	if util.IsContextDone(ctx) {
		return nil
	}

	// Updating a Service is a complicated business

	// spec.resourceVersion is required in order to update object
	newService.ResourceVersion = curService.ResourceVersion

	// The port on each node on which this service is exposed when type=NodePort or LoadBalancer.
	// Usually assigned by the system. If specified, it will be allocated to the service
	// if unused or else creation of the service will fail.
	// Default is to auto-allocate a port if the ServiceType of this Service requires one.
	// More info: https://kubernetes.io/docs/concepts/services-networking/service/#type-nodeport
	if ((curService.Spec.Type == core.ServiceTypeNodePort) && (newService.Spec.Type == core.ServiceTypeNodePort)) ||
		((curService.Spec.Type == core.ServiceTypeLoadBalancer) && (newService.Spec.Type == core.ServiceTypeLoadBalancer)) {
		// No changes in service type and service type assumes NodePort to be allocated.
		// !!! IMPORTANT !!!
		// The same exposed port details can not be changed. This is important limitation
		for i := range newService.Spec.Ports {
			newPort := &newService.Spec.Ports[i]
			for j := range curService.Spec.Ports {
				curPort := &curService.Spec.Ports[j]
				if newPort.Port == curPort.Port {
					// Already have this port specified - reuse all internals,
					// due to limitations with auto-assigned values
					*newPort = *curPort
					w.a.Info("reuse Port %d values", newPort.Port)
					break
				}
			}
		}
	}

	// spec.clusterIP field is immutable, need to use already assigned value
	// From https://kubernetes.io/docs/concepts/services-networking/service/#defining-a-service
	// Kubernetes assigns this Service an IP address (sometimes called the “cluster IP”), which is used by the Service proxies
	// See also https://kubernetes.io/docs/concepts/services-networking/service/#virtual-ips-and-service-proxies
	// You can specify your own cluster IP address as part of a Service creation request. To do this, set the .spec.clusterIP
	newService.Spec.ClusterIP = curService.Spec.ClusterIP

	// spec.healthCheckNodePort field is used with ExternalTrafficPolicy=Local only and is immutable within ExternalTrafficPolicy=Local
	// In case ExternalTrafficPolicy is changed it seems to be irrelevant
	// https://kubernetes.io/docs/tasks/access-application-cluster/create-external-load-balancer/#preserving-the-client-source-ip
	if (curService.Spec.ExternalTrafficPolicy == core.ServiceExternalTrafficPolicyTypeLocal) &&
		(newService.Spec.ExternalTrafficPolicy == core.ServiceExternalTrafficPolicyTypeLocal) {
		newService.Spec.HealthCheckNodePort = curService.Spec.HealthCheckNodePort
	}

	newService.ObjectMeta.Labels = util.MergeStringMapsPreserve(newService.ObjectMeta.Labels, curService.ObjectMeta.Labels)
	newService.ObjectMeta.Annotations = util.MergeStringMapsPreserve(newService.ObjectMeta.Annotations, curService.ObjectMeta.Annotations)
	newService.ObjectMeta.Finalizers = util.MergeStringArrays(newService.ObjectMeta.Finalizers, curService.ObjectMeta.Finalizers)

	// And only now we are ready to actually update the service with new version of the service
	if util.IsContextDone(ctx) {
		return nil
	}
	_, err := w.c.kubeClient.CoreV1().Services(newService.Namespace).Update(newService)

	if err == nil {
		w.a.V(1).
			WithEvent(chi, eventActionUpdate, eventReasonUpdateCompleted).
			WithStatusAction(chi).
			Info("Update Service %s/%s", newService.Namespace, newService.Name)
	} else {
		w.a.WithEvent(chi, eventActionUpdate, eventReasonUpdateFailed).
			WithStatusAction(chi).
			WithStatusError(chi).
			Error("Update Service %s/%s failed with error %v", newService.Namespace, newService.Name, err)
	}

	return err
}

// createService
func (w *worker) createService(ctx context.Context, chi *chop.ClickHouseInstallation, service *core.Service) error {
	if util.IsContextDone(ctx) {
		return nil
	}

	_, err := w.c.kubeClient.CoreV1().Services(service.Namespace).Create(service)

	if err == nil {
		w.a.V(1).
			WithEvent(chi, eventActionCreate, eventReasonCreateCompleted).
			WithStatusAction(chi).
			Info("Create Service %s/%s", service.Namespace, service.Name)
	} else {
		w.a.WithEvent(chi, eventActionCreate, eventReasonCreateFailed).
			WithStatusAction(chi).
			WithStatusError(chi).
			Error("Create Service %s/%s failed with error %v", service.Namespace, service.Name, err)
	}

	return err
}

// reconcileService reconciles core.Service
func (w *worker) reconcileService(ctx context.Context, chi *chop.ClickHouseInstallation, service *core.Service) error {
	w.a.V(2).Info("reconcileService() - start")
	defer w.a.V(2).Info("reconcileService() - end")

	if util.IsContextDone(ctx) {
		return nil
	}

	// Check whether this object already exists
	curService, err := w.c.getService(&service.ObjectMeta, false)

	if curService != nil {
		// We have Service - try to update it
		err = w.updateService(ctx, chi, curService, service)
	}

	if err != nil {
		// Service not found or not updated. Try to recreate
		_ = w.c.deleteServiceIfExists(ctx, service.Namespace, service.Name)
		err = w.createService(ctx, chi, service)
	}

	if err != nil {
		w.a.WithEvent(chi, eventActionReconcile, eventReasonReconcileFailed).
			WithStatusAction(chi).
			WithStatusError(chi).
			Error("FAILED to reconcile Service: %s CHI: %s ", service.Name, chi.Name)
	}

	return err
}

type StatefulSetStatus string

const (
	statefulSetStatusModified StatefulSetStatus = "modified"
	statefulSetStatusNew      StatefulSetStatus = "new"
	statefulSetStatusSame     StatefulSetStatus = "same"
	statefulSetStatusUnknown  StatefulSetStatus = "unknown"
)

func (w *worker) getStatefulSetStatus(statefulSet *apps.StatefulSet) StatefulSetStatus {
	w.a.V(2).Info("getStatefulSetStatus() - start")
	defer w.a.V(2).Info("getStatefulSetStatus() - end")

	// Check whether this object already exists in k8s
	curStatefulSet, err := w.c.getStatefulSet(&statefulSet.ObjectMeta, false)

	if curStatefulSet != nil {
		if _cur, ok := curStatefulSet.Labels[chopmodel.LabelStatefulSetVersion]; ok {
			if _new, _ok := statefulSet.Labels[chopmodel.LabelStatefulSetVersion]; _ok {
				if _cur == _new {
					w.a.Info("INFO StatefulSet ARE EQUAL no reconcile is actually needed")
					return statefulSetStatusSame
				}
			}
		}
		if diff, equal := messagediff.DeepDiff(curStatefulSet.Spec, statefulSet.Spec); equal {
			w.a.Info("INFO StatefulSet ARE DIFFERENT reconcile is required: a:%v m:%v r:%v", diff.Added, diff.Modified, diff.Removed)
			return statefulSetStatusModified
		}
	}

	if apierrors.IsNotFound(err) {
		// StatefulSet not found - even during Update process - try to create it
		return statefulSetStatusNew
	}

	return statefulSetStatusUnknown
}

// reconcileStatefulSet reconciles apps.StatefulSet
func (w *worker) reconcileStatefulSet(ctx context.Context, newStatefulSet *apps.StatefulSet, host *chop.ChiHost) error {
	w.a.V(2).Info("reconcileStatefulSet() - start")
	defer w.a.V(2).Info("reconcileStatefulSet() - end")

	if util.IsContextDone(ctx) {
		return nil
	}

	status := w.getStatefulSetStatus(host.StatefulSet)
	if status == statefulSetStatusSame {
		defer w.a.V(2).Info("reconcileStatefulSet() - no need to reconcile the same StatefulSet")
		return nil
	}

	// Check whether this object already exists in k8s
	curStatefulSet, err := w.c.getStatefulSet(&newStatefulSet.ObjectMeta, false)

	if curStatefulSet != nil {
		// We have StatefulSet - try to update it
		err = w.updateStatefulSet(ctx, curStatefulSet, newStatefulSet, host)
	}

	if apierrors.IsNotFound(err) {
		// StatefulSet not found - even during Update process - try to create it
		err = w.createStatefulSet(ctx, newStatefulSet, host)
	}

	if err != nil {
		w.a.WithEvent(host.CHI, eventActionReconcile, eventReasonReconcileFailed).
			WithStatusAction(host.CHI).
			WithStatusError(host.CHI).
			Error("FAILED to reconcile StatefulSet: %s CHI: %s ", newStatefulSet.Name, host.CHI.Name)
	}

	return err
}

// createStatefulSet
func (w *worker) createStatefulSet(ctx context.Context, statefulSet *apps.StatefulSet, host *chop.ChiHost) error {
	w.a.V(2).Info("createStatefulSet() - start")
	defer w.a.V(2).Info("createStatefulSet() - end")

	if util.IsContextDone(ctx) {
		return nil
	}

	w.a.V(1).
		WithEvent(host.CHI, eventActionCreate, eventReasonCreateStarted).
		WithStatusAction(host.CHI).
		Info("Create StatefulSet %s/%s - started", statefulSet.Namespace, statefulSet.Name)

	err := w.c.createStatefulSet(ctx, statefulSet, host)

	host.CHI.Status.AddedHostsCount++
	_ = w.c.updateCHIObjectStatus(ctx, host.CHI, false)

	if err == nil {
		w.a.V(1).
			WithEvent(host.CHI, eventActionCreate, eventReasonCreateCompleted).
			WithStatusAction(host.CHI).
			Info("Create StatefulSet %s/%s - completed", statefulSet.Namespace, statefulSet.Name)
	} else {
		w.a.WithEvent(host.CHI, eventActionCreate, eventReasonCreateFailed).
			WithStatusAction(host.CHI).
			WithStatusError(host.CHI).
			Error("Create StatefulSet %s/%s - failed with error %v", statefulSet.Namespace, statefulSet.Name, err)
	}

	return err
}

// updateStatefulSet
func (w *worker) updateStatefulSet(ctx context.Context, curStatefulSet, newStatefulSet *apps.StatefulSet, host *chop.ChiHost) error {
	w.a.V(2).Info("updateStatefulSet() - start")
	defer w.a.V(2).Info("updateStatefulSet() - end")

	if util.IsContextDone(ctx) {
		return nil
	}

	namespace := newStatefulSet.Namespace
	name := newStatefulSet.Name

	w.a.V(1).
		WithEvent(host.CHI, eventActionCreate, eventReasonCreateStarted).
		WithStatusAction(host.CHI).
		Info("Update StatefulSet(%s/%s) - started", namespace, name)

	err := w.c.updateStatefulSet(ctx, curStatefulSet, newStatefulSet, host)
	if err == nil {
		host.CHI.Status.UpdatedHostsCount++
		_ = w.c.updateCHIObjectStatus(ctx, host.CHI, false)
		w.a.V(1).
			WithEvent(host.CHI, eventActionUpdate, eventReasonUpdateCompleted).
			WithStatusAction(host.CHI).
			Info("Update StatefulSet(%s/%s) - completed", namespace, name)
		return nil
	}

	w.a.WithEvent(host.CHI, eventActionUpdate, eventReasonUpdateFailed).
		WithStatusAction(host.CHI).
		WithStatusError(host.CHI).
		Error("Update StatefulSet(%s/%s) - failed with error\n---\n%v\n--\nContinue with recreate", namespace, name, err)

	diff, equal := messagediff.DeepDiff(curStatefulSet.Spec, newStatefulSet.Spec)
	w.a.Info("StatefulSet.Spec diff:")
	w.a.Info(util.MessageDiffString(diff, equal))

	err = w.c.deleteStatefulSet(ctx, host)
	err = w.reconcilePersistentVolumeClaims(ctx, host)
	return w.createStatefulSet(ctx, newStatefulSet, host)
}

// reconcilePersistentVolumes
func (w *worker) reconcilePersistentVolumes(ctx context.Context, host *chop.ChiHost) {
	if util.IsContextDone(ctx) {
		return
	}

	w.c.walkPVs(host, func(pv *core.PersistentVolume) {
		pv = w.creator.PreparePersistentVolume(pv, host)
		_ = w.c.updatePersistentVolume(ctx, pv)
	})
}

// reconcilePersistentVolumeClaims
func (w *worker) reconcilePersistentVolumeClaims(ctx context.Context, host *chop.ChiHost) error {
	if util.IsContextDone(ctx) {
		return nil
	}

	namespace := host.Address.Namespace
	w.a.V(2).Info("reconcilePersistentVolumeClaims for host %s/%s - start", namespace, host.Name)
	defer w.a.V(2).Info("reconcilePersistentVolumeClaims for host %s/%s - end", namespace, host.Name)

	host.WalkVolumeMounts(func(volumeMount *core.VolumeMount) {
		if util.IsContextDone(ctx) {
			return
		}

		volumeClaimTemplateName := volumeMount.Name
		volumeClaimTemplate, ok := host.CHI.GetVolumeClaimTemplate(volumeClaimTemplateName)
		if !ok {
			// No this is not a reference to VolumeClaimTemplate
			return
		}

		pvcName := chopmodel.CreatePVCName(host, volumeMount, volumeClaimTemplate)
		w.a.V(2).Info("reconcile volumeMount (%s/%s/%s/%s) - start", namespace, host.Name, volumeMount.Name, pvcName)
		defer w.a.V(2).Info("reconcile volumeMount (%s/%s/%s/%s) - end", namespace, host.Name, volumeMount.Name, pvcName)

		pvc, err := w.c.kubeClient.CoreV1().PersistentVolumeClaims(namespace).Get(pvcName, newGetOptions())
		if err != nil {
			if apierrors.IsNotFound(err) {
				// This is not an error per se, means PVC is not created (yet)?
			} else {
				w.a.Error("ERROR unable to get PVC(%s/%s) err: %v", namespace, pvcName, err)
			}
			return
		}
		w.reconcileResources(ctx, pvc, volumeClaimTemplate)
	})

	return nil
}

// reconcileResources
func (w *worker) reconcileResources(ctx context.Context, pvc *core.PersistentVolumeClaim, template *chop.ChiVolumeClaimTemplate) {
	if util.IsContextDone(ctx) {
		return
	}
	w.reconcileResourcesList(ctx, pvc, pvc.Spec.Resources.Requests, template.Spec.Resources.Requests)
}

// reconcileResourcesList
func (w *worker) reconcileResourcesList(
	ctx context.Context,
	pvc *core.PersistentVolumeClaim,
	pvcResourceList core.ResourceList,
	desiredResourceList core.ResourceList,
) {
	if util.IsContextDone(ctx) {
		return
	}

	var pvcResourceNames []core.ResourceName
	for resourceName := range pvcResourceList {
		pvcResourceNames = append(pvcResourceNames, resourceName)
	}
	var desiredResourceNames []core.ResourceName
	for resourceName := range desiredResourceList {
		desiredResourceNames = append(desiredResourceNames, resourceName)
	}

	//diff, equal := messagediff.DeepDiff(pvcResourceNames, desiredResourceNames)

	resourceNames := intersect.Simple(pvcResourceNames, desiredResourceNames)
	for _, resourceName := range resourceNames.([]interface{}) {
		w.reconcileResource(ctx, pvc, pvcResourceList, desiredResourceList, resourceName.(core.ResourceName))
	}
}

// reconcileResourcesList
func (w *worker) reconcileResource(
	ctx context.Context,
	pvc *core.PersistentVolumeClaim,
	pvcResourceList core.ResourceList,
	desiredResourceList core.ResourceList,
	resourceName core.ResourceName,
) {
	w.a.V(2).Info("reconcileResource(%s/%s/%s) - start", pvc.Namespace, pvc.Name, resourceName)
	defer w.a.V(2).Info("reconcileResource(%s/%s/%s) - end", pvc.Namespace, pvc.Name, resourceName)

	if util.IsContextDone(ctx) {
		return
	}

	var ok bool
	if (pvcResourceList == nil) || (desiredResourceList == nil) {
		return
	}

	var pvcResourceQuantity resource.Quantity
	var desiredResourceQuantity resource.Quantity
	if pvcResourceQuantity, ok = pvcResourceList[resourceName]; !ok {
		return
	}
	if desiredResourceQuantity, ok = desiredResourceList[resourceName]; !ok {
		return
	}

	if pvcResourceQuantity.Equal(desiredResourceQuantity) {
		return
	}

	w.a.V(2).Info("reconcileResource(%s/%s/%s) - unequal requests, want to update", pvc.Namespace, pvc.Name, resourceName)
	pvcResourceList[resourceName] = desiredResourceList[resourceName]
	_, err := w.c.kubeClient.CoreV1().PersistentVolumeClaims(pvc.Namespace).Update(pvc)
	if err != nil {
		w.a.Error("unable to reconcileResource(%s/%s/%s) err: %v", pvc.Namespace, pvc.Name, resourceName, err)
		return
	}
}
