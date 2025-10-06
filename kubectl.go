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

	// Lazy wrapper for listing pods (used when new workloads appear)
	k.podListerOnce.Do(func() {
		logger.Debug("Initializing pod lister function")
		lister := factory.Core().V1().Pods().Lister()
		k.podListerFunc = func() ([]*corev1.Pod, error) {
			return lister.Pods(k.Namespace).List(labels.Everything())
		}
	})

	// Processing pod events
	logger.Debug("Adding pod event handlers")
	podInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { k.onPodEvent(ctx, obj) },
		UpdateFunc: func(_, newObj interface{}) { k.onPodEvent(ctx, newObj) },
	})

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
		// Sync of all needed caches will happen inside startWorkloadWatchers through the same factory
	) {
		logger.Error("Cache sync failed")
		return fmt.Errorf("cache not synced")
	}
	logger.Info("Cache sync completed successfully")

	// After initial sync - find existing workloads and their pods
	logger.Debug("Scanning existing workloads")
	k.scanExistingWorkloads(ctx, factory)

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
		// Rescan pods when new workload appears - look for pods belonging to our workloads only
		logger.Debug("Rescanning pods due to new workload discovery")
		k.scanPodsForWorkloads(ctx)
	}

	// Setup all workload watchers using workloads package
	workloads.SetupAllWorkloadWatchers(factory, k.Namespace, k.TrackingID, k.TrackingIDExact, addIfMatch, delUID, rescan)
}

// scanExistingWorkloads - finds existing workloads with needed tracking-id and their pods
func (k *KubeLogStreamer) scanExistingWorkloads(ctx context.Context, factory informers.SharedInformerFactory) {
	addToCache := func(uid string) {
		k.ownerCacheMu.Lock()
		k.topLevelUIDs[uid] = struct{}{}
		k.ownerCacheMu.Unlock()
	}

	foundWorkloads := workloads.ScanAllExistingWorkloads(factory, k.Namespace, k.TrackingID, k.TrackingIDExact, addToCache)

	logger.WithField("count", len(foundWorkloads)).Info("Found existing matching workloads")

	// Now find pods for these workloads
	if len(foundWorkloads) > 0 {
		k.scanPodsForWorkloads(ctx)
	}
}

// scanPodsForWorkloads - finds pods belonging to found workloads
func (k *KubeLogStreamer) scanPodsForWorkloads(ctx context.Context) {
	logger.Debug("Scanning pods for found workloads")
	if k.podListerFunc == nil {
		logger.Warning("Pod lister not ready, skipping pod scan")
		return
	}

	pods, err := k.podListerFunc()
	if err != nil {
		logger.WithError(err).Warning("Failed to list pods for workloads")
		return
	}

	processedCount := 0
	for _, pod := range pods {
		if k.podBelongsToTargets(ctx, pod) {
			k.onPodWithExistingFlag(ctx, pod, true)
			processedCount++
		}
	}

	logger.WithFields(logrus.Fields{
		"total_pods":     len(pods),
		"processed_pods": processedCount,
	}).Info("Completed pod scan for workloads")
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
			logger.WithField("stream", streamKey).Debug("Cleaning up log stream")
			k.mu.Lock()
			delete(k.activeStreams, key)
			k.mu.Unlock()
		}()

		// Use LogStreamer to handle the actual log streaming
		if err := k.logStreamer.StartLogStream(ctx, pod.Name, container, isExistingPod); err != nil {
			logger.WithField("stream", streamKey).WithError(err).Warning("Log stream ended with error")
		}
	}()
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
