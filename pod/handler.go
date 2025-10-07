// Package pod provides pod event handling functionality
package pod

import (
	"argocd-watcher/streaming"
	"argocd-watcher/workloadFinder"
	"context"
	"fmt"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
)

// OwnershipChecker checks if a pod belongs to tracked workloads
type OwnershipChecker interface {
	PodBelongsToTargets(ctx context.Context, pod *corev1.Pod) bool
}

// Handler handles pod events and coordinates log streaming
type Handler struct {
	namespace          string
	workloadFinder     *workloadFinder.WorkloadFinder
	streamCoordinator  *streaming.Coordinator
	logger             *logrus.Logger
}

// NewHandler creates a new pod handler
func NewHandler(namespace string, workloadFinder *workloadFinder.WorkloadFinder, streamCoordinator *streaming.Coordinator, logger *logrus.Logger) *Handler {
	return &Handler{
		namespace:         namespace,
		workloadFinder:    workloadFinder,
		streamCoordinator: streamCoordinator,
		logger:            logger,
	}
}

// OnPodEvent handles pod add/update events
func (h *Handler) OnPodEvent(ctx context.Context, obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		h.logger.WithField("type", fmt.Sprintf("%T", obj)).Warning("Received non-pod object in pod event handler")
		return
	}
	h.logger.WithField("pod", pod.Name).Debug("Pod event received")
	h.ProcessPod(ctx, pod, false)
}

// ProcessPod processes a pod and starts log streams for its containers
func (h *Handler) ProcessPod(ctx context.Context, pod *corev1.Pod, isExistingPod bool) {
	if pod.Namespace != h.namespace {
		h.logger.WithFields(logrus.Fields{
			"pod":                pod.Name,
			"expected_namespace": h.namespace,
		}).Debug("Skipping pod: wrong namespace")
		return
	}

	// Check if belongs to our top-level workloads
	if !h.workloadFinder.PodBelongsToTargets(ctx, pod, h.namespace) {
		h.logger.WithField("pod", pod.Name).Debug("Pod does not belong to target workloads, skipping")
		return
	}

	h.logger.WithField("pod", pod.Name).Info("Processing pod - belongs to target workloads")

	// Process init containers
	for _, cs := range pod.Status.InitContainerStatuses {
		h.logger.WithFields(logrus.Fields{
			"pod":       pod.Name,
			"container": cs.Name,
			"restarts":  cs.RestartCount,
			"type":      "init",
		}).Debug("Found init container")
		key := streaming.PodContainerKey{
			PodName:       pod.Name,
			Namespace:     pod.Namespace,
			ContainerName: cs.Name,
			RestartCount:  cs.RestartCount,
		}
		h.streamCoordinator.MaybeStartLogStream(ctx, pod, cs.Name, key, isExistingPod)
	}

	// Process regular containers
	for _, cs := range pod.Status.ContainerStatuses {
		h.logger.WithFields(logrus.Fields{
			"pod":       pod.Name,
			"container": cs.Name,
			"restarts":  cs.RestartCount,
			"type":      "regular",
		}).Debug("Found container")
		key := streaming.PodContainerKey{
			PodName:       pod.Name,
			Namespace:     pod.Namespace,
			ContainerName: cs.Name,
			RestartCount:  cs.RestartCount,
		}
		h.streamCoordinator.MaybeStartLogStream(ctx, pod, cs.Name, key, isExistingPod)
	}
}

// PodBelongsToTargets implements the ownership checking interface
func (h *Handler) PodBelongsToTargets(ctx context.Context, pod *corev1.Pod) bool {
	return h.workloadFinder.PodBelongsToTargets(ctx, pod, h.namespace)
}
