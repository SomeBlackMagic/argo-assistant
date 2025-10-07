// kubectl.go
package main

import (
	"argocd-watcher/workloads"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
)

const trackingAnnotationKey = "argocd.argoproj.io/tracking-id"

type PodContainerKey struct {
	PodName       string
	Namespace     string
	ContainerName string
	RestartCount  int32
}

type KubeLogStreamer struct {
	Client     *kubernetes.Clientset
	RestConfig *rest.Config
	Namespace  string

	TrackingID      string
	TrackingIDExact bool

	Out io.Writer

	mu            sync.Mutex
	activeStreams map[PodContainerKey]struct{}

	ownerCacheMu sync.Mutex
	topLevelUIDs map[string]struct{} // UIDs of top-level workloads (Deployment/StatefulSet/DaemonSet/Job/CronJob)

	// for rescanning pods when new workloads appear
	podListerOnce sync.Once
	podListerFunc func() ([]*corev1.Pod, error)

	// log streamer for formatting output
	logStreamer *LogStreamer

	// track last seen event time per pod
	eventTimestampMu sync.Mutex
	lastEventTime    map[string]time.Time // podName -> last event timestamp
}

func createKubernetesClient() (*kubernetes.Clientset, *rest.Config, error) {
	logger.Info("Initializing Kubernetes client...")

	// InCluster → KUBECONFIG
	cfg, err := rest.InClusterConfig()
	if err != nil {
		logger.Debug("Failed to load in-cluster config, trying KUBECONFIG...")
		kubeconfig := os.Getenv("KUBECONFIG")
		if kubeconfig == "" {
			if home, _ := os.UserHomeDir(); home != "" {
				kubeconfig = home + "/.kube/config"
			}
		}
		logger.WithField("kubeconfig", kubeconfig).Debug("Using kubeconfig file")
		cfg, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
		if err != nil {
			logger.WithError(err).Error("Failed to build config from kubeconfig")
			return nil, nil, err
		}
	} else {
		logger.Info("Successfully loaded in-cluster configuration")
	}

	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		logger.WithError(err).Error("Failed to create Kubernetes clientset")
		return nil, nil, err
	}

	logger.Info("Kubernetes client successfully initialized")
	return cs, cfg, nil
}

func NewKubeLogStreamer(namespace, trackingID string, exact bool, out io.Writer) *KubeLogStreamer {
	logger.Info("Creating KubeLogStreamer")

	kubeClient, kubeConfig, err := createKubernetesClient()
	if err != nil {
		logger.WithError(err).Fatal("Failed to create Kubernetes client")
		os.Exit(1)
	}

	// Set logger for workloads package
	workloads.SetLogger(logger)

	streamer := &KubeLogStreamer{
		Client:          kubeClient,
		RestConfig:      kubeConfig,
		Namespace:       namespace,
		TrackingID:      trackingID,
		TrackingIDExact: exact,
		Out:             out,
		activeStreams:   make(map[PodContainerKey]struct{}),
		topLevelUIDs:    make(map[string]struct{}),
		logStreamer:     NewLogStreamer(kubeClient, namespace, out),
		lastEventTime:   make(map[string]time.Time),
	}

	logger.Info("KubeLogStreamer successfully created")
	return streamer
}

// StreamLogsByTrackingID: sets up informers for workloads (waiting for them to appear) and for pods,
// then works until the context is cancelled.
func (k *KubeLogStreamer) StreamLogsByTrackingID(ctx context.Context) error {
	logger.WithFields(logrus.Fields{
		"namespace":  k.Namespace,
		"trackingID": k.TrackingID,
	}).Info("Starting log streaming")

	// Shared factory with namespace restriction
	logger.Debug("Creating shared informer factory")
	factory := informers.NewSharedInformerFactoryWithOptions(
		k.Client,
		time.Minute*10,
		informers.WithNamespace(k.Namespace),
	)

	// Pod informer
	logger.Debug("Setting up pod informer")
	podInf := factory.Core().V1().Pods().Informer()
	if podInf == nil {
		logger.Error("Failed to create pod informer")
		return fmt.Errorf("failed to create pod informer")
	}

	// Event informer
	logger.Debug("Setting up event informer")
	eventInf := factory.Core().V1().Events().Informer()
	if eventInf == nil {
		logger.Error("Failed to create event informer")
		return fmt.Errorf("failed to create event informer")
	}

	// Lazy wrapper for listing pods (used when new workloads appear)
	k.podListerOnce.Do(func() {
		logger.Debug("Initializing pod lister function")
		lister := factory.Core().V1().Pods().Lister()
		if lister == nil {
			logger.Error("Failed to create pod lister")
			return
		}
		k.podListerFunc = func() ([]*corev1.Pod, error) {
			return lister.Pods(k.Namespace).List(labels.Everything())
		}
	})

	// Processing pod events
	logger.Debug("Adding pod event handlers")
	_, err := podInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			defer func() {
				if r := recover(); r != nil {
					logger.WithField("panic", r).Error("Panic in pod add handler")
				}
			}()
			k.onPodEvent(ctx, obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			defer func() {
				if r := recover(); r != nil {
					logger.WithField("panic", r).Error("Panic in pod update handler")
				}
			}()
			k.onPodEvent(ctx, newObj)
		},
	})
	if err != nil {
		logger.WithError(err).Error("Failed to add pod event handler")
		return fmt.Errorf("failed to add pod event handler: %w", err)
	}

	// Processing Kubernetes events
	logger.Debug("Adding event handlers")
	_, err = eventInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			defer func() {
				if r := recover(); r != nil {
					logger.WithField("panic", r).Error("Panic in event add handler")
				}
			}()
			k.onEvent(ctx, obj)
		},
		UpdateFunc: func(_, newObj interface{}) {
			defer func() {
				if r := recover(); r != nil {
					logger.WithField("panic", r).Error("Panic in event update handler")
				}
			}()
			k.onEvent(ctx, newObj)
		},
	})
	if err != nil {
		logger.WithError(err).Error("Failed to add event handler")
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	// Start watchers for workloads and subscribe to their changes
	logger.Debug("Starting workload watchers")
	k.startWorkloadWatchers(ctx, factory)

	stopCh := make(chan struct{})
	defer close(stopCh)

	logger.Info("Starting informer factory")
	factory.Start(stopCh)

	logger.Info("Waiting for cache sync...")
	if !cache.WaitForCacheSync(ctx.Done(),
		podInf.HasSynced,
		eventInf.HasSynced,
		// Sync of all needed caches will happen inside startWorkloadWatchers through the same factory
	) {
		logger.Error("Cache sync failed")
		return fmt.Errorf("cache not synced")
	}
	logger.Info("Cache sync completed successfully")

	// After initial sync - find existing workloads and their pods
	logger.Debug("Scanning existing workloads")
	if err := k.scanExistingWorkloads(ctx, factory); err != nil {
		logger.WithError(err).Error("Failed to scan existing workloads")
		return fmt.Errorf("failed to scan existing workloads: %w", err)
	}

	logger.Info("Log streaming active, waiting for context cancellation")
	<-ctx.Done()
	logger.Info("Context cancelled, stopping log streaming")
	return ctx.Err()
}

// Sets up handlers for Deployments/StatefulSets/DaemonSets/Jobs/CronJobs.
// When objects with target tracking-id appear/update - adds their UID to the top-level set and rescans pods.
func (k *KubeLogStreamer) startWorkloadWatchers(ctx context.Context, factory informers.SharedInformerFactory) {
	addIfMatch := func(obj metav1.Object) bool {
		logger.WithFields(logrus.Fields{
			"workload":    obj.GetNamespace() + "/" + obj.GetName(),
			"tracking-id": obj.GetAnnotations()[trackingAnnotationKey],
		}).Info("Found matching workload")

		k.ownerCacheMu.Lock()
		_, existed := k.topLevelUIDs[string(obj.GetUID())]
		k.topLevelUIDs[string(obj.GetUID())] = struct{}{}
		k.ownerCacheMu.Unlock()

		if !existed {
			logger.WithFields(logrus.Fields{
				"uid":      string(obj.GetUID()),
				"workload": obj.GetNamespace() + "/" + obj.GetName(),
			}).Debug("Added new top-level UID for workload")
		}
		return !existed
	}
	delUID := func(obj metav1.Object) {
		logger.WithFields(logrus.Fields{
			"uid":      string(obj.GetUID()),
			"workload": obj.GetNamespace() + "/" + obj.GetName(),
		}).Debug("Removing UID from tracking")
		k.ownerCacheMu.Lock()
		delete(k.topLevelUIDs, string(obj.GetUID()))
		k.ownerCacheMu.Unlock()
	}

	rescan := func() {
		// Rescan pods when a new workload appears - look for pods belonging to our workloads only
		logger.Debug("Rescanning pods due to new workload discovery")
		k.scanPodsForWorkloads(ctx)
	}

	// Set up all workload watchers using the workloads package
	workloads.SetupAllWorkloadWatchers(factory, k.Namespace, k.TrackingID, k.TrackingIDExact, addIfMatch, delUID, rescan)
}

// scanExistingWorkloads - finds existing workloads with necessary tracking-id and their pods
func (k *KubeLogStreamer) scanExistingWorkloads(ctx context.Context, factory informers.SharedInformerFactory) error {
	addToCache := func(uid string) {
		k.ownerCacheMu.Lock()
		k.topLevelUIDs[uid] = struct{}{}
		k.ownerCacheMu.Unlock()
	}

	foundWorkloads := workloads.ScanAllExistingWorkloads(factory, k.Namespace, k.TrackingID, k.TrackingIDExact, addToCache)

	logger.WithField("count", len(foundWorkloads)).Info("Found existing matching workloads")

	// Now find pods for these workloads
	if len(foundWorkloads) > 0 {
		if err := k.scanPodsForWorkloads(ctx); err != nil {
			logger.WithError(err).Error("Failed to scan pods for existing workloads")
			return fmt.Errorf("failed to scan pods for existing workloads: %w", err)
		}
	}

	return nil
}

// scanPodsForWorkloads - finds pods belonging to found workloads
func (k *KubeLogStreamer) scanPodsForWorkloads(ctx context.Context) error {
	logger.Debug("Scanning pods for found workloads")
	if k.podListerFunc == nil {
		logger.Warning("Pod lister not ready, skipping pod scan")
		return fmt.Errorf("pod lister not ready")
	}

	pods, err := k.podListerFunc()
	if err != nil {
		logger.WithError(err).Error("Failed to list pods for workloads")
		return fmt.Errorf("failed to list pods for workloads: %w", err)
	}

	processedCount := 0
	for _, pod := range pods {
		// Check context before processing each pod
		select {
		case <-ctx.Done():
			logger.WithError(ctx.Err()).Debug("Context cancelled during pod scanning")
			return ctx.Err()
		default:
		}

		if k.podBelongsToTargets(ctx, pod) {
			k.onPodWithExistingFlag(ctx, pod, true)
			processedCount++
		}
	}

	logger.WithFields(logrus.Fields{
		"total_pods":     len(pods),
		"processed_pods": processedCount,
	}).Info("Completed pod scan for workloads")

	return nil
}

// onPodEvent - wrapper function
func (k *KubeLogStreamer) onPodEvent(ctx context.Context, obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		logger.WithField("type", fmt.Sprintf("%T", obj)).Warning("Received non-pod object in pod event handler")
		return
	}
	logger.WithField("pod", pod.Name).Debug("Pod event received")
	k.onPod(ctx, pod)
}

// onPod - filters pod by owners and starts logs for all containers
func (k *KubeLogStreamer) onPod(ctx context.Context, pod *corev1.Pod) {
	k.onPodWithExistingFlag(ctx, pod, false)
}

// onPodWithExistingFlag - internal function for processing pods with existing pod flag
func (k *KubeLogStreamer) onPodWithExistingFlag(ctx context.Context, pod *corev1.Pod, isExistingPod bool) {
	if pod.Namespace != k.Namespace {
		logger.WithFields(logrus.Fields{
			"pod":                pod.Name,
			"expected_namespace": k.Namespace,
		}).Debug("Skipping pod: wrong namespace")
		return
	}

	// Check if belongs to our top-level workloads
	if !k.podBelongsToTargets(ctx, pod) {
		logger.WithField("pod", pod.Name).Debug("Pod does not belong to target workloads, skipping")
		return
	}

	logger.WithField("pod", pod.Name).Info("Processing pod - belongs to target workloads")

	// initContainers
	for _, cs := range pod.Status.InitContainerStatuses {
		logger.WithFields(logrus.Fields{
			"pod":       pod.Name,
			"container": cs.Name,
			"restarts":  cs.RestartCount,
			"type":      "init",
		}).Debug("Found init container")
		key := PodContainerKey{PodName: pod.Name, Namespace: pod.Namespace, ContainerName: cs.Name, RestartCount: cs.RestartCount}
		k.maybeStartLogStream(ctx, pod, cs.Name, key, isExistingPod)
	}
	// regular containers
	for _, cs := range pod.Status.ContainerStatuses {
		logger.WithFields(logrus.Fields{
			"pod":       pod.Name,
			"container": cs.Name,
			"restarts":  cs.RestartCount,
			"type":      "regular",
		}).Debug("Found container")
		key := PodContainerKey{PodName: pod.Name, Namespace: pod.Namespace, ContainerName: cs.Name, RestartCount: cs.RestartCount}
		k.maybeStartLogStream(ctx, pod, cs.Name, key, isExistingPod)
	}
}

func (k *KubeLogStreamer) maybeStartLogStream(ctx context.Context, pod *corev1.Pod, container string, key PodContainerKey, isExistingPod bool) {
	streamKey := pod.Name + "/" + container

	k.mu.Lock()
	if _, exists := k.activeStreams[key]; exists {
		k.mu.Unlock()
		logger.WithFields(logrus.Fields{
			"stream":   streamKey,
			"restarts": key.RestartCount,
		}).Debug("Log stream already active")
		return
	}
	k.activeStreams[key] = struct{}{}
	k.mu.Unlock()

	logger.WithFields(logrus.Fields{
		"stream":   streamKey,
		"restarts": key.RestartCount,
	}).Info("Starting new log stream")

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.WithFields(logrus.Fields{
					"stream": streamKey,
					"panic":  r,
				}).Error("Panic in log stream goroutine")
			}

			logger.WithField("stream", streamKey).Debug("Cleaning up log stream")
			k.mu.Lock()
			delete(k.activeStreams, key)
			k.mu.Unlock()
		}()

		// Use LogStreamer to handle the actual log streaming
		if err := k.logStreamer.StartLogStream(ctx, pod.Name, container, isExistingPod); err != nil {
			// Check if the error is due to context cancellation
			if ctx.Err() != nil {
				logger.WithFields(logrus.Fields{
					"stream":      streamKey,
					"context_err": ctx.Err().Error(),
				}).Debug("Log stream ended due to context cancellation")
			} else {
				logger.WithFields(logrus.Fields{
					"stream": streamKey,
					"error":  err.Error(),
				}).Error("Log stream ended with error")
			}
		} else {
			logger.WithField("stream", streamKey).Debug("Log stream ended successfully")
		}
	}()
}

// onEvent handles Kubernetes events and outputs them for pods that belong to our tracked workloads
func (k *KubeLogStreamer) onEvent(ctx context.Context, obj interface{}) {
	event, ok := obj.(*corev1.Event)
	if !ok {
		logger.WithField("type", fmt.Sprintf("%T", obj)).Warning("Received non-event object in event handler")
		return
	}

	// Only process events related to pods
	if event.InvolvedObject.Kind != "Pod" {
		return
	}

	// Get the pod name from the event
	podName := event.InvolvedObject.Name

	// Check if we have already seen this exact event (based on timestamp and message)
	eventKey := podName
	k.eventTimestampMu.Lock()
	lastTime, exists := k.lastEventTime[eventKey]

	// Skip if we've seen a newer or equal event for this pod
	if exists && !event.LastTimestamp.Time.After(lastTime) {
		k.eventTimestampMu.Unlock()
		return
	}

	k.lastEventTime[eventKey] = event.LastTimestamp.Time
	k.eventTimestampMu.Unlock()

	// Check if the pod belongs to our tracked workloads
	// We need to fetch the pod to check ownership
	pod, err := k.Client.CoreV1().Pods(k.Namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		// Pod might have been deleted, but we still want to show the event
		logger.WithField("pod", podName).WithError(err).Debug("Could not fetch pod for event, skipping ownership check")
		return
	}

	if !k.podBelongsToTargets(ctx, pod) {
		return
	}

	// Format and output the event
	timestamp := event.LastTimestamp.Time.Format("2006-01-02T15:04:05Z")
	eventType := event.Type
	reason := event.Reason
	message := event.Message
	count := event.Count

	// Map event to log level
	logLevel := MapEventToLogLevel(event)

	// Log with appropriate level
	logEntry := logger.WithFields(logrus.Fields{
		"pod":       podName,
		"timestamp": timestamp,
		"type":      eventType,
		"reason":    reason,
		"count":     count,
		"source":    "event",
	})

	switch logLevel {
	case Debug:
		logEntry.Debug(message)
	case Info:
		logEntry.Info(message)
	case Warn:
		logEntry.Warn(message)
	case Error:
		logEntry.Error(message)
	default:
		logEntry.Info(message)
	}
}

// traverse ownerReferences up to top-level and check inclusion in k.topLevelUIDs
func (k *KubeLogStreamer) podBelongsToTargets(ctx context.Context, pod *corev1.Pod) bool {
	//logger.WithField("pod", pod.Name).Debug("Checking ownership chain")

	visited := map[string]bool{}
	var (
		kind string = "Pod"
		uid  string = string(pod.UID)
		ns          = pod.Namespace
		name        = pod.Name
	)

	for {
		key := kind + "/" + uid
		if visited[key] {
			logger.WithField("key", key).Warning("Circular reference detected in ownership chain")
			return false
		}
		visited[key] = true

		logger.WithFields(logrus.Fields{
			"kind": kind,
			"name": ns + "/" + name,
			"uid":  uid,
		}).Debug("Checking if resource is a top-level target")

		k.ownerCacheMu.Lock()
		_, ok := k.topLevelUIDs[uid]
		k.ownerCacheMu.Unlock()
		if ok {
			logger.WithFields(logrus.Fields{
				"kind": kind,
				"name": ns + "/" + name,
			}).Debug("Found matching target workload")
			return true
		}

		var owner *metav1.OwnerReference
		ownerRefs := getOwnerRefs(kind, ns, name, pod)
		for i := range ownerRefs {
			or := &ownerRefs[i]
			if or.Controller != nil && *or.Controller {
				owner = or
				break
			}
		}
		if owner == nil {
			logger.WithFields(logrus.Fields{
				"kind": kind,
				"name": ns + "/" + name,
			}).Debug("No controller owner found")
			return false
		}

		logger.WithFields(logrus.Fields{
			"owner_kind": owner.Kind,
			"owner_name": owner.Name,
			"owner_uid":  owner.UID,
		}).Debug("Found controller owner")

		switch owner.Kind {
		case "ReplicaSet":
			// Check if context is already cancelled before making API call
			select {
			case <-ctx.Done():
				logger.WithField("replicaset", ns+"/"+owner.Name).Debug("Context cancelled, skipping ReplicaSet lookup")
				return false
			default:
			}

			rs, err := k.Client.AppsV1().ReplicaSets(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				// Check for specific error types
				if ctx.Err() != nil {
					logger.WithFields(logrus.Fields{
						"replicaset":  ns + "/" + owner.Name,
						"context_err": ctx.Err().Error(),
					}).Debug("Context cancelled during ReplicaSet lookup")
					return false
				}
				logger.WithField("replicaset", ns+"/"+owner.Name).WithError(err).Warning("Failed to get ReplicaSet")
				return false
			}
			kind, uid, name = "ReplicaSet", string(rs.UID), rs.Name
			logger.WithField("replicaset", ns+"/"+name).Debug("Moving up ownership chain to ReplicaSet")
			if hasTopOwnerUID(k, rs.OwnerReferences) {
				logger.WithField("replicaset", ns+"/"+name).Debug("ReplicaSet has top-level owner")
				return true
			}
			if or := firstController(rs.OwnerReferences); or != nil {
				owner = or
			} else {
				logger.WithField("replicaset", ns+"/"+name).Debug("ReplicaSet has no controller owner")
				return false
			}
			continue
		case "StatefulSet":
			// Check if context is already cancelled before making API call
			select {
			case <-ctx.Done():
				logger.WithField("statefulset", ns+"/"+owner.Name).Debug("Context cancelled, skipping StatefulSet lookup")
				return false
			default:
			}

			ss, err := k.Client.AppsV1().StatefulSets(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				// Check for specific error types
				if ctx.Err() != nil {
					logger.WithFields(logrus.Fields{
						"statefulset": ns + "/" + owner.Name,
						"context_err": ctx.Err().Error(),
					}).Debug("Context cancelled during StatefulSet lookup")
					return false
				}
				logger.WithField("statefulset", ns+"/"+owner.Name).WithError(err).Warning("Failed to get StatefulSet")
				return false
			}
			result := k.uidIsTop(string(ss.UID))
			logger.WithFields(logrus.Fields{
				"statefulset": ns + "/" + owner.Name,
				"is_target":   result,
			}).Debug("Checked StatefulSet target status")
			return result
		case "DaemonSet":
			// Check if context is already cancelled before making API call
			select {
			case <-ctx.Done():
				logger.WithField("daemonset", ns+"/"+owner.Name).Debug("Context cancelled, skipping DaemonSet lookup")
				return false
			default:
			}

			ds, err := k.Client.AppsV1().DaemonSets(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				// Check for specific error types
				if ctx.Err() != nil {
					logger.WithFields(logrus.Fields{
						"daemonset":   ns + "/" + owner.Name,
						"context_err": ctx.Err().Error(),
					}).Debug("Context cancelled during DaemonSet lookup")
					return false
				}
				logger.WithField("daemonset", ns+"/"+owner.Name).WithError(err).Warning("Failed to get DaemonSet")
				return false
			}
			result := k.uidIsTop(string(ds.UID))
			logger.WithFields(logrus.Fields{
				"daemonset": ns + "/" + owner.Name,
				"is_target": result,
			}).Debug("Checked DaemonSet target status")
			return result
		case "Job":
			// Check if context is already cancelled before making API call
			select {
			case <-ctx.Done():
				logger.WithField("job", ns+"/"+owner.Name).Debug("Context cancelled, skipping Job lookup")
				return false
			default:
			}

			job, err := k.Client.BatchV1().Jobs(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				// Check for specific error types
				if ctx.Err() != nil {
					logger.WithFields(logrus.Fields{
						"job":         ns + "/" + owner.Name,
						"context_err": ctx.Err().Error(),
					}).Debug("Context cancelled during Job lookup")
					return false
				}
				logger.WithField("job", ns+"/"+owner.Name).WithError(err).Warning("Failed to get Job")
				return false
			}
			if hasTopOwnerUID(k, job.OwnerReferences) {
				logger.WithField("job", ns+"/"+owner.Name).Debug("Job has top-level owner")
				return true
			}
			if or := firstController(job.OwnerReferences); or != nil {
				owner = or
				continue
			}
			result := k.uidIsTop(string(job.UID))
			logger.WithFields(logrus.Fields{
				"job":       ns + "/" + owner.Name,
				"is_target": result,
			}).Debug("Checked Job target status")
			return result
		case "Deployment":
			// Check if context is already cancelled before making API call
			select {
			case <-ctx.Done():
				logger.WithField("deployment", ns+"/"+owner.Name).Debug("Context cancelled, skipping Deployment lookup")
				return false
			default:
			}

			dep, err := k.Client.AppsV1().Deployments(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				// Check for specific error types
				if ctx.Err() != nil {
					logger.WithFields(logrus.Fields{
						"deployment":  ns + "/" + owner.Name,
						"context_err": ctx.Err().Error(),
					}).Debug("Context cancelled during Deployment lookup")
					return false
				}
				logger.WithField("deployment", ns+"/"+owner.Name).WithError(err).Warning("Failed to get Deployment")
				return false
			}
			result := k.uidIsTop(string(dep.UID))
			logger.WithFields(logrus.Fields{
				"deployment": ns + "/" + owner.Name,
				"is_target":  result,
			}).Debug("Checked Deployment target status")
			return result
		case "CronJob":
			// Check if context is already cancelled before making API call
			select {
			case <-ctx.Done():
				logger.WithField("cronjob", ns+"/"+owner.Name).Debug("Context cancelled, skipping CronJob lookup")
				return false
			default:
			}

			cj, err := k.Client.BatchV1().CronJobs(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				// Check for specific error types
				if ctx.Err() != nil {
					logger.WithFields(logrus.Fields{
						"cronjob":     ns + "/" + owner.Name,
						"context_err": ctx.Err().Error(),
					}).Debug("Context cancelled during CronJob lookup")
					return false
				}
				logger.WithField("cronjob", ns+"/"+owner.Name).WithError(err).Warning("Failed to get CronJob")
				return false
			}
			result := k.uidIsTop(string(cj.UID))
			logger.WithFields(logrus.Fields{
				"cronjob":   ns + "/" + owner.Name,
				"is_target": result,
			}).Debug("Checked CronJob target status")
			return result
		default:
			logger.WithFields(logrus.Fields{
				"owner_kind": owner.Kind,
				"owner_name": ns + "/" + owner.Name,
			}).Warning("Unknown owner kind")
			return false
		}
	}
}

func (k *KubeLogStreamer) uidIsTop(uid string) bool {
	k.ownerCacheMu.Lock()
	defer k.ownerCacheMu.Unlock()
	_, ok := k.topLevelUIDs[uid]
	return ok
}

func hasTopOwnerUID(k *KubeLogStreamer, ors []metav1.OwnerReference) bool {
	k.ownerCacheMu.Lock()
	defer k.ownerCacheMu.Unlock()
	for _, or := range ors {
		if _, ok := k.topLevelUIDs[string(or.UID)]; ok {
			return true
		}
	}
	return false
}

func firstController(ors []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range ors {
		if ors[i].Controller != nil && *ors[i].Controller {
			return &ors[i]
		}
	}
	return nil
}

func getOwnerRefs(kind, ns, name string, pod *corev1.Pod) []metav1.OwnerReference {
	if kind == "Pod" {
		return pod.OwnerReferences
	}
	return nil
}
