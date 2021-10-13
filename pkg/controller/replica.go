/*
Copyright 2021 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeClientSet "k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/apis/azuredisk/v1alpha1"
	azClientSet "sigs.k8s.io/azuredisk-csi-driver/pkg/apis/client/clientset/versioned"
	consts "sigs.k8s.io/azuredisk-csi-driver/pkg/azureconstants"
	"sigs.k8s.io/azuredisk-csi-driver/pkg/azureutils"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

const (
	deletionPollingInterval = time.Duration(10) * time.Second
)

type ReconcileReplica struct {
	client                     client.Client
	azVolumeClient             azClientSet.Interface
	kubeClient                 kubeClientSet.Interface
	namespace                  string
	controllerSharedState      *SharedState
	deletionMap                sync.Map
	cleanUpMap                 sync.Map
	mutexLocks                 sync.Map
	timeUntilGarbageCollection time.Duration
}

var _ reconcile.Reconciler = &ReconcileReplica{}

func (r *ReconcileReplica) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
	azVolumeAttachment, err := azureutils.GetAzVolumeAttachment(ctx, r.client, r.azVolumeClient, request.Name, request.Namespace, true)
	if errors.IsNotFound(err) {
		klog.Infof("AzVolumeAttachment (%s) has been successfully deleted.", request.Name)
		return reconcile.Result{}, nil
	} else if err != nil {
		klog.Errorf("failed to fetch AzVolumeAttachment (%s): %v", request.Name, err)
		return reconcile.Result{Requeue: true}, err
	}

	if azVolumeAttachment.Spec.RequestedRole == v1alpha1.PrimaryRole {
		// Deletion Event
		if criDeletionRequested(&azVolumeAttachment.ObjectMeta) && volumeDetachRequested(azVolumeAttachment) {
			// If primary attachment is marked for deletion, queue garbage collection for replica attachments
			r.triggerGarbageCollection(azVolumeAttachment.Spec.UnderlyingVolume)
		} else {
			// If not, cancel scheduled garbage collection if there is one enqueued
			r.cancelGarbageCollection(azVolumeAttachment.Spec.UnderlyingVolume)

			// If promotion event, create a replacement replica
			if isAttached(azVolumeAttachment) && azVolumeAttachment.Status.Detail.Role != azVolumeAttachment.Spec.RequestedRole {
				if err := r.manageReplicas(ctx, azVolumeAttachment.Spec.UnderlyingVolume); err != nil {
					return reconcile.Result{Requeue: true}, err
				}
			}
		}
	} else {
		// create a replacement replica if replica attachment failed or promoted
		if criDeletionRequested(&azVolumeAttachment.ObjectMeta) {
			if azVolumeAttachment.Status.State == v1alpha1.DetachmentFailed {
				if err := azureutils.UpdateCRIWithRetry(ctx, nil, r.client, r.azVolumeClient, azVolumeAttachment, func(obj interface{}) error {
					azVolumeAttachment := obj.(*v1alpha1.AzVolumeAttachment)
					_, err = updateState(ctx, azVolumeAttachment, v1alpha1.ForceDetachPending, normalUpdate)
					return err
				}); err != nil {
					return reconcile.Result{Requeue: true}, err
				}
			}
			if azVolumeAttachment.Annotations == nil || !metav1.HasAnnotation(azVolumeAttachment.ObjectMeta, consts.CleanUpAnnotation) {
				go func() {
					// wait for replica AzVolumeAttachment deletion
					conditionFunc := func() (bool, error) {
						var tmp v1alpha1.AzVolumeAttachment
						err := r.client.Get(ctx, request.NamespacedName, &tmp)
						if errors.IsNotFound(err) {
							return true, nil
						}

						return false, err
					}
					_ = wait.PollImmediateInfinite(deletionPollingInterval, conditionFunc)

					// retry replica management with exponential backoff until success
					conditionFunc = func() (bool, error) {
						err := r.manageReplicas(ctx, azVolumeAttachment.Spec.UnderlyingVolume)
						return true, err
					}
					_ = wait.ExponentialBackoffWithContext(ctx, wait.Backoff{Duration: defaultRetryDuration, Factor: defaultRetryFactor, Steps: defaultRetrySteps}, conditionFunc)
				}()
			}
		} else if azVolumeAttachment.Status.State == v1alpha1.AttachmentFailed {
			// if attachment failed for replica AzVolumeAttachment, delete the CRI so that replace replica AzVolumeAttachment can be created.
			if err := r.client.Delete(ctx, azVolumeAttachment); err != nil {
				return reconcile.Result{Requeue: true}, err
			}
		}
	}
	return reconcile.Result{}, nil
}

func (r *ReconcileReplica) triggerGarbageCollection(volumeName string) {
	emptyCtx := context.TODO()
	deletionCtx, cancelFunc := context.WithCancel(emptyCtx)
	_, _ = r.cleanUpMap.LoadOrStore(volumeName, cancelFunc)
	klog.Infof("garbage collection of AzVolumeAttachments for AzVolume (%s) scheduled in %s.", volumeName, DefaultTimeUntilGarbageCollection.String())

	go func(ctx context.Context) {
		select {
		case <-ctx.Done():
			klog.Infof("garbage collection for AzVolume (%s) cancelled", volumeName)
			return
		case <-time.After(r.timeUntilGarbageCollection):
			klog.Infof("Initiating garbage collection for AzVolume (%s)", volumeName)
			v, _ := r.mutexLocks.LoadOrStore(volumeName, &sync.RWMutex{})
			volumeLock := v.(*sync.RWMutex)
			volumeLock.Lock()
			_, _ = cleanUpAzVolumeAttachmentByVolume(ctx, r, volumeName, "replicaController", all, detachAndDeleteCRI)
			volumeLock.Unlock()
			r.mutexLocks.Delete(volumeName)
			r.controllerSharedState.unmarkVolumeVisited(volumeName)
		}
	}(deletionCtx)
}

func (r *ReconcileReplica) cancelGarbageCollection(volumeName string) {
	v, ok := r.cleanUpMap.LoadAndDelete(volumeName)
	if ok {
		cancelFunc := v.(context.CancelFunc)
		cancelFunc()
	}
}

func (r *ReconcileReplica) manageReplicas(ctx context.Context, volumeName string) error {
	// this is to prevent multiple sync volume operation to be performed on a single volume concurrently as it can create or delete more attachments than necessary
	v, _ := r.mutexLocks.LoadOrStore(volumeName, &sync.RWMutex{})
	volumeLock := v.(*sync.RWMutex)
	volumeLock.Lock()
	defer volumeLock.Unlock()

	azVolume, err := azureutils.GetAzVolume(ctx, r.client, r.azVolumeClient, volumeName, r.namespace, true)
	if errors.IsNotFound(err) {
		klog.Infof("Aborting replica management... volume (%s) does not exist", volumeName)
		return nil
	} else if err != nil {
		klog.Errorf("failed to get AzVolume (%s): %v", volumeName, err)
		return err
	}
	if !isCreated(azVolume) {
		return status.Errorf(codes.Aborted, "azVolume (%s) has no underlying volume object", azVolume.Name)
	}

	azVolumeAttachments, err := getAzVolumeAttachmentsForVolume(ctx, r.client, volumeName, replicaOnly)
	if err != nil {
		klog.Errorf("failed to list AzVolumeAttachment: %v", err)
		return err
	}

	desiredReplicaCount, currentReplicaCount := azVolume.Spec.MaxMountReplicaCount, len(azVolumeAttachments)
	klog.Infof("control number of replicas for volume (%s): desired=%d,\tcurrent:%d", azVolume.Spec.UnderlyingVolume, desiredReplicaCount, currentReplicaCount)

	// if the azVolume is marked deleted, do no create more azvolumeattachment objects
	if azVolume.DeletionTimestamp == nil && desiredReplicaCount > currentReplicaCount {
		klog.Infof("Need %d more replicas for volume (%s)", desiredReplicaCount-currentReplicaCount, azVolume.Spec.UnderlyingVolume)
		if azVolume.Status.Detail == nil || azVolume.Status.State == v1alpha1.VolumeDeleting || azVolume.Status.State == v1alpha1.VolumeDeleted || azVolume.Status.Detail.ResponseObject == nil {
			// underlying volume does not exist, so volume attachment cannot be made
			return nil
		}
		if err = r.createReplicas(ctx, min(defaultMaxReplicaUpdateCount, desiredReplicaCount-currentReplicaCount), azVolume.Name, azVolume.Status.Detail.ResponseObject.VolumeID); err != nil {
			klog.Errorf("failed to create %d replicas for volume (%s): %v", desiredReplicaCount-currentReplicaCount, azVolume.Spec.UnderlyingVolume, err)
			return err
		}
	}
	return nil
}

func (r *ReconcileReplica) getNodesForReplica(ctx context.Context, volumeName string, pods []string, numReplica int) ([]string, error) {
	var err error
	if pods == nil {
		pods, err = r.controllerSharedState.getPodsFromVolume(volumeName)
		if err != nil {
			return nil, err
		}
	}

	volumes, err := r.controllerSharedState.getVolumesForPods(pods...)
	if err != nil {
		return nil, err
	}

	nodes, err := getRankedNodesForReplicaAttachments(ctx, r, volumes)
	if err != nil {
		return nil, err
	}

	replicaNodes, err := getNodesWithReplica(ctx, r, volumeName)
	if err != nil {
		return nil, err
	}

	skipSet := map[string]bool{}
	for _, replicaNode := range replicaNodes {
		skipSet[replicaNode] = true
	}

	filtered := []string{}
	numFiltered := 0
	for _, node := range nodes {
		if numFiltered >= numReplica {
			break
		}
		if skipSet[node] {
			continue
		}
		filtered = append(filtered, node)
		numFiltered++
	}

	return filtered, nil
}

func (r *ReconcileReplica) createReplicas(ctx context.Context, numReplica int, volumeName, volumeID string) error {
	// if volume is scheduled for clean up, skip replica creation
	if _, cleanUpScheduled := r.cleanUpMap.Load(volumeName); cleanUpScheduled {
		return nil
	}

	// get pods linked to the volume
	pods, err := r.controllerSharedState.getPodsFromVolume(volumeName)
	if err != nil {
		return err
	}

	// acquire per-pod lock to be released upon creation of replica AzVolumeAttachment CRIs
	for _, pod := range pods {
		v, _ := r.controllerSharedState.podLocks.LoadOrStore(pod, &sync.Mutex{})
		podLock := v.(*sync.Mutex)
		podLock.Lock()
		defer podLock.Unlock()
	}

	nodes, err := r.getNodesForReplica(ctx, volumeName, pods, numReplica)
	if err != nil {
		klog.Errorf("failed to get a list of nodes for replica attachment: %v", err)
		return err
	}

	for _, node := range nodes {
		if err := createReplicaAzVolumeAttachment(ctx, r, volumeID, node); err != nil {
			klog.Errorf("failed to create replica AzVolumeAttachment for volume %s: %v", volumeName, err)
			return err
		}
	}
	return nil
}

func NewReplicaController(mgr manager.Manager, azVolumeClient azClientSet.Interface, kubeClient kubeClientSet.Interface, namespace string, controllerSharedState *SharedState) (*ReconcileReplica, error) {
	logger := mgr.GetLogger().WithValues("controller", "replica")
	reconciler := ReconcileReplica{
		client:                     mgr.GetClient(),
		azVolumeClient:             azVolumeClient,
		kubeClient:                 kubeClient,
		namespace:                  namespace,
		controllerSharedState:      controllerSharedState,
		deletionMap:                sync.Map{},
		cleanUpMap:                 sync.Map{},
		mutexLocks:                 sync.Map{},
		timeUntilGarbageCollection: DefaultTimeUntilGarbageCollection,
	}
	c, err := controller.New("replica-controller", mgr, controller.Options{
		MaxConcurrentReconciles: 10,
		Reconciler:              &reconciler,
		Log:                     logger,
	})

	if err != nil {
		klog.Errorf("Failed to create replica controller. Error: (%v)", err)
		return nil, err
	}

	klog.V(2).Info("Starting to watch AzVolumeAttachments.")

	err = c.Watch(&source.Kind{Type: &v1alpha1.AzVolumeAttachment{}}, &handler.EnqueueRequestForObject{}, predicate.Funcs{
		CreateFunc: func(e event.CreateEvent) bool {
			azVolumeAttachment, ok := e.Object.(*v1alpha1.AzVolumeAttachment)
			if ok && azVolumeAttachment.Spec.RequestedRole == v1alpha1.PrimaryRole {
				return true
			}
			return false
		},
		UpdateFunc: func(e event.UpdateEvent) bool {
			return true
		},
		GenericFunc: func(e event.GenericEvent) bool {
			return false
		},
		DeleteFunc: func(e event.DeleteEvent) bool {
			return true
		},
	})
	if err != nil {
		klog.Errorf("Failed to watch AzVolumeAttachment. Error: %v", err)
		return nil, err
	}

	klog.V(2).Info("Controller set-up successful.")
	return &reconciler, nil
}

func (r *ReconcileReplica) getClient() client.Client {
	return r.client
}

func (r *ReconcileReplica) getAzClient() azClientSet.Interface {
	return r.azVolumeClient
}
