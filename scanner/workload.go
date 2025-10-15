// Package scanner provides workload and pod scanning functionality
package scanner

import (
	"context"
	"fmt"
	"github.com/SomeBlackMagic/argo-assistant/informer"
	"github.com/SomeBlackMagic/argo-assistant/workloadFinder"
	"github.com/SomeBlackMagic/argo-assistant/workloads"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
)

const trackingAnnotationKey = "argocd.argoproj.io/tracking-id"

// PodProcessor defines the interface for processing pods
type PodProcessor interface {
	ProcessPod(ctx context.Context, pod *corev1.Pod, isExistingPod bool)
}

// Scanner scans for workloads and their associated pods
type Scanner struct {
	namespace       string
	trackingID      string
	trackingIDExact bool
	workloadFinder  *workloadFinder.WorkloadFinder
	podListerFunc   informer.PodListerFunc
	podProcessor    PodProcessor
	logger          *logrus.Logger
}

// NewScanner creates a new workload scanner
func NewScanner(namespace, trackingID string, trackingIDExact bool, workloadFinder *workloadFinder.WorkloadFinder, podListerFunc informer.PodListerFunc, podProcessor PodProcessor, logger *logrus.Logger) *Scanner {
	return &Scanner{
		namespace:       namespace,
		trackingID:      trackingID,
		trackingIDExact: trackingIDExact,
		workloadFinder:  workloadFinder,
		podListerFunc:   podListerFunc,
		podProcessor:    podProcessor,
		logger:          logger,
	}
}

// StartWorkloadWatchers sets up handlers for Deployments/StatefulSets/DaemonSets/Jobs/CronJobs
func (s *Scanner) StartWorkloadWatchers(ctx context.Context, factory informers.SharedInformerFactory) {
	addIfMatch := func(obj metav1.Object) bool {
		s.logger.WithFields(logrus.Fields{
			"workload":    obj.GetNamespace() + "/" + obj.GetName(),
			"tracking-id": obj.GetAnnotations()[trackingAnnotationKey],
		}).Info("Found matching workload")

		existed := !s.workloadFinder.AddTopLevelUID(string(obj.GetUID()))

		if !existed {
			s.logger.WithFields(logrus.Fields{
				"uid":      string(obj.GetUID()),
				"workload": obj.GetNamespace() + "/" + obj.GetName(),
			}).Debug("Added new top-level UID for workload")
		}
		return !existed
	}

	delUID := func(obj metav1.Object) {
		s.logger.WithFields(logrus.Fields{
			"uid":      string(obj.GetUID()),
			"workload": obj.GetNamespace() + "/" + obj.GetName(),
		}).Debug("Removing UID from tracking")
		s.workloadFinder.RemoveTopLevelUID(string(obj.GetUID()))
	}

	rescan := func() {
		// Rescan pods when a new workload appears
		s.logger.Debug("Rescanning pods due to new workload discovery")
		s.ScanPodsForWorkloads(ctx)
	}

	// Set up all workload watchers using the workloads package
	workloads.SetupAllWorkloadWatchers(factory, s.namespace, s.trackingID, s.trackingIDExact, addIfMatch, delUID, rescan)
}

// ScanExistingWorkloads finds existing workloads with necessary tracking-id and their pods
func (s *Scanner) ScanExistingWorkloads(ctx context.Context, factory informers.SharedInformerFactory) error {
	addToCache := func(uid string) {
		s.workloadFinder.AddTopLevelUID(uid)
	}

	foundWorkloads := workloads.ScanAllExistingWorkloads(factory, s.namespace, s.trackingID, s.trackingIDExact, addToCache)

	s.logger.WithField("count", len(foundWorkloads)).Info("Found existing matching workloads")

	// Now find pods for these workloads
	if len(foundWorkloads) > 0 {
		if err := s.ScanPodsForWorkloads(ctx); err != nil {
			if err.Error() != "context canceled" {
				s.logger.WithError(err).Error("Failed to scan pods for existing workloads")
				return fmt.Errorf("failed to scan pods for existing workloads: %w", err)
			}
		}
	}

	return nil
}

// ScanPodsForWorkloads finds pods belonging to found workloads
func (s *Scanner) ScanPodsForWorkloads(ctx context.Context) error {
	s.logger.Debug("Scanning pods for found workloads")
	if s.podListerFunc == nil {
		s.logger.Warning("Pod lister not ready, skipping pod scan")
		return fmt.Errorf("pod lister not ready")
	}

	pods, err := s.podListerFunc()
	if err != nil {
		s.logger.WithError(err).Error("Failed to list pods for workloads")
		return fmt.Errorf("failed to list pods for workloads: %w", err)
	}

	processedCount := 0
	for _, pod := range pods {
		// Check context before processing each pod
		select {
		case <-ctx.Done():
			s.logger.WithError(ctx.Err()).Debug("Context cancelled during pod scanning")
			return ctx.Err()
		default:
		}

		if s.workloadFinder.PodBelongsToTargets(ctx, pod, s.namespace) {
			s.podProcessor.ProcessPod(ctx, pod, true)
			processedCount++
		}
	}

	s.logger.WithFields(logrus.Fields{
		"total_pods":     len(pods),
		"processed_pods": processedCount,
	}).Info("Completed pod scan for workloads")

	return nil
}
