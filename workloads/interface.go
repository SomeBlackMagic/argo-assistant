// workloads/interface.go
package workloads

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// WorkloadHandler defines the interface for handling different workload types
type WorkloadHandler interface {
	// SetupInformer sets up the informer for this workload type
	SetupInformer(factory informers.SharedInformerFactory, handlers WorkloadEventHandlers) cache.SharedIndexInformer

	// ListExisting lists existing workloads of this type from the factory
	ListExisting(factory informers.SharedInformerFactory, namespace string) ([]metav1.Object, error)

	// GetTypeName returns the name of the workload type
	GetTypeName() string
}

// WorkloadEventHandlers contains callbacks for workload events
type WorkloadEventHandlers struct {
	AddIfMatch func(metav1.Object) bool
	DelUID     func(metav1.Object)
	Rescan     func()
}

// TrackingMatcher is a function that checks if a tracking ID matches
type TrackingMatcher func(string) bool

// WorkloadManager manages all workload handlers
type WorkloadManager struct {
	handlers []WorkloadHandler
}

// NewWorkloadManager creates a new workload manager with all workload types
func NewWorkloadManager() *WorkloadManager {
	return &WorkloadManager{
		handlers: []WorkloadHandler{
			NewDeploymentHandler(),
			NewStatefulSetHandler(),
			NewDaemonSetHandler(),
			NewJobHandler(),
			NewCronJobHandler(),
		},
	}
}

// SetupAllInformers sets up informers for all workload types
func (wm *WorkloadManager) SetupAllInformers(factory informers.SharedInformerFactory, handlers WorkloadEventHandlers) []cache.SharedIndexInformer {
	var informers []cache.SharedIndexInformer
	for _, handler := range wm.handlers {
		informer := handler.SetupInformer(factory, handlers)
		informers = append(informers, informer)
	}
	return informers
}

// ScanAllExisting scans for existing workloads of all types
func (wm *WorkloadManager) ScanAllExisting(factory informers.SharedInformerFactory, namespace string, matcher TrackingMatcher, addToCache func(string)) []metav1.Object {
	var allWorkloads []metav1.Object

	for _, handler := range wm.handlers {
		workloads, err := handler.ListExisting(factory, namespace)
		if err != nil {
			logger.WithError(err).Warningf("Failed to list existing %s", handler.GetTypeName())
			continue
		}

		for _, workload := range workloads {
			if annotations := workload.GetAnnotations(); annotations != nil {
				if v, ok := annotations["argocd.argoproj.io/tracking-id"]; ok && matcher(v) {
					logger.WithFields(logger.Fields{
						handler.GetTypeName(): workload.GetNamespace() + "/" + workload.GetName(),
						"tracking-id": v,
					}).Info("Found existing matching " + handler.GetTypeName())
					addToCache(string(workload.GetUID()))
					allWorkloads = append(allWorkloads, workload)
				}
			}
		}
	}

	return allWorkloads
}
