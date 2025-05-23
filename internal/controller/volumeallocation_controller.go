/*
Copyright 2025.

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
	"fmt"
	"sort"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	crdv1 "github.com/MohamedGouaouri/ms-app-controller/api/v1"
	resource "k8s.io/apimachinery/pkg/api/resource"
)

// VolumeAllocationReconciler reconciles a VolumeAllocation object
type VolumeAllocationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=crd.cs.phd.uqtr,resources=volumeallocations,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=crd.cs.phd.uqtr,resources=volumeallocations/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=crd.cs.phd.uqtr,resources=volumeallocations/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the VolumeAllocation object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.20.4/pkg/reconcile
func (r *VolumeAllocationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := logf.FromContext(ctx)

	// TODO(user): your logic here
	// Fetch allocation request
	var allocationRequest crdv1.VolumeAllocation
	if err := r.Get(ctx, req.NamespacedName, &allocationRequest); err != nil {
		logger.Error(err, "unable to fetch VolumeAllocation request")
		// Ignore not-found errors (e.g., if the resource was deleted)
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Create PVC
	annotations := make(map[string]string)
	edgeTopologyName := types.NamespacedName{
		Name:      allocationRequest.Spec.EdgeNetworkTopology,
		Namespace: allocationRequest.Namespace,
	}
	volumeAllocation := r.DistributedVolumeAllocation(ctx, allocationRequest, edgeTopologyName, 3) // TODO, i need to change kmax to be configurable
	annotation := ""
	for edge, allocation := range volumeAllocation {
		annotation += fmt.Sprintf("%s:%d", edge, allocation) + ","
	}
	annotations["volumeallocation"] = annotation
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        allocationRequest.Name + "-pvc",
			Namespace:   allocationRequest.Namespace,
			Annotations: annotations,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteOnce,
			},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(allocationRequest.Spec.VolumeSize),
				},
			},
			StorageClassName: &allocationRequest.Spec.StorageClassName,
		},
	}
	// Set the controller as owner and controller
	if err := ctrl.SetControllerReference(&allocationRequest, pvc, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	// Create the PVC if it doesn't exist
	var existingPVC corev1.PersistentVolumeClaim
	err := r.Get(ctx, client.ObjectKey{Name: pvc.Name, Namespace: pvc.Namespace}, &existingPVC)
	if err != nil && client.IgnoreNotFound(err) == nil {
		logger.Info("Creating PVC", "pvc", pvc.Name)
		if err := r.Create(ctx, pvc); err != nil {
			logger.Error(err, "failed to create PVC")
			return ctrl.Result{}, err
		}
	} else if err != nil {
		logger.Error(err, "error checking for existing PVC")
		return ctrl.Result{}, err
	} else {
		logger.Info("PVC already exists", "pvc", pvc.Name)
	}

	return ctrl.Result{}, nil
}

func (r *VolumeAllocationReconciler) DistributedVolumeAllocation(ctx context.Context, allocationRequest crdv1.VolumeAllocation, edgeTopologyName types.NamespacedName, kMax int) map[string]int {
	logger := logf.FromContext(ctx)
	var edgeNetworkTopology crdv1.EdgeNetworkTopology

	if err := r.Get(ctx, edgeTopologyName, &edgeNetworkTopology); err != nil {
		logger.Error(err, "unable to fetch EdgeNetworkTopology")
		return nil
	}

	volumeQuantity, err := resource.ParseQuantity(allocationRequest.Spec.VolumeSize)
	if err != nil {
		logger.Error(err, "unable to parse volume size")
		return nil
	}
	blocks := int(volumeQuantity.Value() / (1024 * 1024)) // Convert bytes to MB

	result := make(map[string]int)

	eStar := allocationRequest.Spec.MicroservicePlacement
	remaining := blocks

	for k := 0; k <= kMax; k++ {
		neighbors := KHopNeighbors(edgeNetworkTopology.Spec.Edges, eStar, k)

		// For now, sort neighbors alphabetically (could sort by affinity later)
		sort.Strings(neighbors)

		for _, e := range neighbors {
			available := r.GetAvailableDisk(e)
			if remaining > 0 && available > 0 {
				alloc := min(remaining, available)
				result[e] += alloc
				UpdateDiskAvailability(e, available-alloc)
				remaining -= alloc
			}
		}

		if remaining == 0 {
			break
		}
	}

	// if remaining > 0 {
	// result["cloud"] += remaining // fallback to cloud
	// }

	return result
}

// Find K-hop neighbors of an edge node
func KHopNeighbors(edges []crdv1.EdgeNode, start string, k int) []string {
	// BFS to find k-hop neighbors
	visited := make(map[string]bool)
	current := []string{start}
	visited[start] = true

	for depth := 0; depth < k; depth++ {
		next := []string{}
		for _, node := range current {
			for _, e := range edges {
				if e.Name == node {
					for _, link := range e.Links {
						if !visited[link.EdgeNodeRef] {
							visited[link.EdgeNodeRef] = true
							next = append(next, link.EdgeNodeRef)
						}
					}
				}
			}
		}
		current = next
	}

	return current
}

func (r *VolumeAllocationReconciler) GetAvailableDisk(node string) int {
	// TODO: Hook into actual storage monitoring or state cache
	return 10240 // Placeholder: 10Gi in MB
}

func UpdateDiskAvailability(node string, newAvailable int) {
	// TODO: Update the disk availability in the actual state or cache
}

// SetupWithManager sets up the controller with the Manager.
func (r *VolumeAllocationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&crdv1.VolumeAllocation{}).
		Named("volumeallocation").
		Complete(r)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
