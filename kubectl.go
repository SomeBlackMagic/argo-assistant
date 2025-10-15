// kubectl.go
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/SomeBlackMagic/argo-assistant/client"
	"github.com/SomeBlackMagic/argo-assistant/eventsPipe"
	"github.com/SomeBlackMagic/argo-assistant/informer"
	"github.com/SomeBlackMagic/argo-assistant/logsPipe"
	"github.com/SomeBlackMagic/argo-assistant/pod"
	"github.com/SomeBlackMagic/argo-assistant/scanner"
	"github.com/SomeBlackMagic/argo-assistant/streaming"
	"github.com/SomeBlackMagic/argo-assistant/workloadFinder"
	"github.com/SomeBlackMagic/argo-assistant/workloads"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

type KubeLogStreamer struct {
	Client     *kubernetes.Clientset
	RestConfig *rest.Config
	Namespace  string

	TrackingID      string
	TrackingIDExact bool

	Out io.Writer

	// Components
	informerManager   *informer.Manager
	streamCoordinator *streaming.Coordinator
	podHandler        *pod.Handler
	workloadScanner   *scanner.Scanner
	eventHandler      *eventsPipe.EventHandler
	workloadFinder    *workloadFinder.WorkloadFinder
}

func NewKubeLogStreamer(namespace, trackingID string, exact bool, out io.Writer) *KubeLogStreamer {
	logger.Info("Creating KubeLogStreamer")

	kubeClient, kubeConfig, err := client.CreateKubernetesClient(logger)
	if err != nil {
		logger.WithError(err).Fatal("Failed to create Kubernetes client")
		os.Exit(1)
	}

	// Set logger for workloads package
	workloads.SetLogger(logger)

	// Create base components
	wf := workloadFinder.NewWorkloadFinder(kubeClient, logger)
	logStreamer := logsPipe.NewLogStreamer(kubeClient, namespace, out, logger)
	streamCoordinator := streaming.NewCoordinator(logStreamer, logger)
	podHandler := pod.NewHandler(namespace, wf, streamCoordinator, logger)
	informerMgr := informer.NewManager(kubeClient, namespace, logger)

	streamer := &KubeLogStreamer{
		Client:            kubeClient,
		RestConfig:        kubeConfig,
		Namespace:         namespace,
		TrackingID:        trackingID,
		TrackingIDExact:   exact,
		Out:               out,
		informerManager:   informerMgr,
		streamCoordinator: streamCoordinator,
		podHandler:        podHandler,
		eventHandler:      eventsPipe.NewEventHandler(kubeClient, namespace, logger),
		workloadFinder:    wf,
	}

	logger.Info("KubeLogStreamer successfully created")
	return streamer
}

// StreamLogsByTrackingID sets up informers for workloads and pods, then streams logs until context is cancelled
func (k *KubeLogStreamer) StreamLogsByTrackingID(ctx context.Context) error {
	logger.WithFields(map[string]interface{}{
		"namespace":  k.Namespace,
		"trackingID": k.TrackingID,
	}).Info("Starting log streaming")

	// Set up pod informer with handlers
	if err := k.informerManager.SetupPodInformer(
		func(obj interface{}) {
			defer recoverPanic("pod add handler")
			k.podHandler.OnPodEvent(ctx, obj)
		},
		func(_, newObj interface{}) {
			defer recoverPanic("pod update handler")
			k.podHandler.OnPodEvent(ctx, newObj)
		},
	); err != nil {
		return err
	}

	// Setup event informer with handlers
	if err := k.informerManager.SetupEventInformer(
		func(obj interface{}) {
			defer recoverPanic("event add handler")
			k.onEvent(ctx, obj)
		},
		func(_, newObj interface{}) {
			defer recoverPanic("event update handler")
			k.onEvent(ctx, newObj)
		},
	); err != nil {
		return err
	}

	// Create pod lister for scanner
	podLister := k.informerManager.CreatePodLister()

	// Initialize the workload scanner with all dependencies
	k.workloadScanner = scanner.NewScanner(
		k.Namespace,
		k.TrackingID,
		k.TrackingIDExact,
		k.workloadFinder,
		podLister,
		k.podHandler,
		logger,
	)

	// Start workload watchers
	logger.Debug("Starting workload watchers")
	k.workloadScanner.StartWorkloadWatchers(ctx, k.informerManager.Factory())

	// Start informers
	k.informerManager.Start()
	defer k.informerManager.Stop()

	// Wait for cache sync
	if err := k.informerManager.WaitForCacheSync(ctx); err != nil {
		return err
	}

	// Scan existing workloads and their pods
	logger.Debug("Scanning existing workloads")
	if err := k.workloadScanner.ScanExistingWorkloads(ctx, k.informerManager.Factory()); err != nil {
		logger.WithError(err).Error("Failed to scan existing workloads")
		return fmt.Errorf("failed to scan existing workloads: %w", err)
	}

	logger.Info("Log streaming active, waiting for context cancellation")
	<-ctx.Done()
	logger.Info("Context cancelled, stopping log streaming")
	return ctx.Err()
}

func recoverPanic(handler string) {
	if r := recover(); r != nil {
		logger.WithField("panic", r).Errorf("Panic in %s", handler)
	}
}

// onEvent handles Kubernetes events and outputs them for pods that belong to our tracked workloads
func (k *KubeLogStreamer) onEvent(ctx context.Context, obj interface{}) {
	k.eventHandler.HandleEvent(ctx, obj, k)
}

// PodBelongsToTargets implements eventsPipe.PodOwnershipChecker interface
func (k *KubeLogStreamer) PodBelongsToTargets(ctx context.Context, pod *corev1.Pod) bool {
	return k.podHandler.PodBelongsToTargets(ctx, pod)
}
