// kubectl.go
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	rest "k8s.io/client-go/rest"
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
	topLevelUIDs map[string]struct{} // UID'ы топовых ворклоадов (Deployment/StatefulSet/DaemonSet/Job/CronJob)

	// для повторного сканирования подов при появлении новых ворклоадов
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

// StreamLogsByTrackingID: поднимает информеры по ворклоадам (ожидание появления) и по подам,
// затем работает, пока не остановят контекст.
func (k *KubeLogStreamer) StreamLogsByTrackingID(ctx context.Context) error {
	logger.WithFields(logrus.Fields{
		"namespace":  k.Namespace,
		"trackingID": k.TrackingID,
	}).Info("Starting log streaming")

	// Общая фабрика с ограничением по namespace
	logger.Debug("Creating shared informer factory")
	factory := informers.NewSharedInformerFactoryWithOptions(
		k.Client,
		time.Minute*10,
		informers.WithNamespace(k.Namespace),
	)

	// Под-информер
	logger.Debug("Setting up pod informer")
	podInf := factory.Core().V1().Pods().Informer()

	// Ленивая обёртка для листинга подов (используется при появлении новых ворклоадов)
	k.podListerOnce.Do(func() {
		logger.Debug("Initializing pod lister function")
		lister := factory.Core().V1().Pods().Lister()
		k.podListerFunc = func() ([]*corev1.Pod, error) {
			return lister.Pods(k.Namespace).List(labels.Everything())
		}
	})

	// Обработка событий по подам
	logger.Debug("Adding pod event handlers")
	podInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    func(obj interface{}) { k.onPodEvent(ctx, obj) },
		UpdateFunc: func(_, newObj interface{}) { k.onPodEvent(ctx, newObj) },
	})

	// Поднимаем вотчеры по ворклоадам и подписываемся на их изменения
	logger.Debug("Starting workload watchers")
	k.startWorkloadWatchers(ctx, factory)

	stopCh := make(chan struct{})
	defer close(stopCh)

	logger.Info("Starting informer factory")
	factory.Start(stopCh)

	logger.Info("Waiting for cache sync...")
	if !cache.WaitForCacheSync(ctx.Done(),
		podInf.HasSynced,
		// Синк всех нужных кэшей произойдёт внутри startWorkloadWatchers через ту же фабрику
	) {
		logger.Error("Cache sync failed")
		return fmt.Errorf("cache not synced")
	}
	logger.Info("Cache sync completed successfully")

	// После первичного синка — найдем существующие workload'ы и их поды
	logger.Debug("Scanning existing workloads")
	k.scanExistingWorkloads(ctx, factory)

	logger.Info("Log streaming active, waiting for context cancellation")
	<-ctx.Done()
	logger.Info("Context cancelled, stopping log streaming")
	return ctx.Err()
}

// Поднимает обработчики на Deployments/StatefulSets/DaemonSets/Jobs/CronJobs.
// При появлении/обновлении объектов с целевым tracking-id — добавляет их UID в топ-набор и пересканывает поды.
func (k *KubeLogStreamer) startWorkloadWatchers(ctx context.Context, factory informers.SharedInformerFactory) {
	match := k.makeTrackingMatcher()

	addIfMatch := func(obj metav1.Object) bool {
		annotations := obj.GetAnnotations()
		if annotations == nil {
			return false
		}
		if v, ok := annotations[trackingAnnotationKey]; ok && match(v) {
			logger.WithFields(logrus.Fields{
				"workload":    obj.GetNamespace() + "/" + obj.GetName(),
				"tracking-id": v,
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
		return false
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
		// Перескан подов при появлении нового ворклоада - ищем только поды принадлежащие нашим workload'ам
		logger.Debug("Rescanning pods due to new workload discovery")
		k.scanPodsForWorkloads(ctx)
	}

	// Deployments
	logger.Debug("Setting up Deployment watcher")
	depInf := factory.Apps().V1().Deployments().Informer()
	depInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("deployment", o.GetNamespace()+"/"+o.GetName()).Debug("Deployment added")
				if addIfMatch(o) {
					rescan()
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if o, ok := newObj.(metav1.Object); ok {
				logger.WithField("deployment", o.GetNamespace()+"/"+o.GetName()).Debug("Deployment updated")
				if addIfMatch(o) {
					rescan()
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("deployment", o.GetNamespace()+"/"+o.GetName()).Debug("Deployment deleted")
				delUID(o)
			}
		},
	})

	// StatefulSets
	logger.Debug("Setting up StatefulSet watcher")
	stsInf := factory.Apps().V1().StatefulSets().Informer()
	stsInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("statefulset", o.GetNamespace()+"/"+o.GetName()).Debug("StatefulSet added")
				if addIfMatch(o) {
					rescan()
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if o, ok := newObj.(metav1.Object); ok {
				logger.WithField("statefulset", o.GetNamespace()+"/"+o.GetName()).Debug("StatefulSet updated")
				if addIfMatch(o) {
					rescan()
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("statefulset", o.GetNamespace()+"/"+o.GetName()).Debug("StatefulSet deleted")
				delUID(o)
			}
		},
	})

	// DaemonSets
	logger.Debug("Setting up DaemonSet watcher")
	dsInf := factory.Apps().V1().DaemonSets().Informer()
	dsInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("daemonset", o.GetNamespace()+"/"+o.GetName()).Debug("DaemonSet added")
				if addIfMatch(o) {
					rescan()
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if o, ok := newObj.(metav1.Object); ok {
				logger.WithField("daemonset", o.GetNamespace()+"/"+o.GetName()).Debug("DaemonSet updated")
				if addIfMatch(o) {
					rescan()
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("daemonset", o.GetNamespace()+"/"+o.GetName()).Debug("DaemonSet deleted")
				delUID(o)
			}
		},
	})

	// Jobs
	logger.Debug("Setting up Job watcher")
	jobInf := factory.Batch().V1().Jobs().Informer()
	jobInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("job", o.GetNamespace()+"/"+o.GetName()).Debug("Job added")
				if addIfMatch(o) {
					rescan()
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if o, ok := newObj.(metav1.Object); ok {
				logger.WithField("job", o.GetNamespace()+"/"+o.GetName()).Debug("Job updated")
				if addIfMatch(o) {
					rescan()
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("job", o.GetNamespace()+"/"+o.GetName()).Debug("Job deleted")
				delUID(o)
			}
		},
	})

	// CronJobs
	logger.Debug("Setting up CronJob watcher")
	cjInf := factory.Batch().V1().CronJobs().Informer()
	cjInf.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("cronjob", o.GetNamespace()+"/"+o.GetName()).Debug("CronJob added")
				if addIfMatch(o) {
					rescan()
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if o, ok := newObj.(metav1.Object); ok {
				logger.WithField("cronjob", o.GetNamespace()+"/"+o.GetName()).Debug("CronJob updated")
				if addIfMatch(o) {
					rescan()
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("cronjob", o.GetNamespace()+"/"+o.GetName()).Debug("CronJob deleted")
				delUID(o)
			}
		},
	})
}

// scanExistingWorkloads - ищет существующие workload'ы с нужным tracking-id и их поды
func (k *KubeLogStreamer) scanExistingWorkloads(ctx context.Context, factory informers.SharedInformerFactory) {
	match := k.makeTrackingMatcher()
	var foundWorkloads []metav1.Object

	// Проверяем Deployments
	deployments, err := factory.Apps().V1().Deployments().Lister().Deployments(k.Namespace).List(labels.Everything())
	if err != nil {
		logger.WithError(err).Warning("Failed to list existing deployments")
	} else {
		for _, dep := range deployments {
			if annotations := dep.GetAnnotations(); annotations != nil {
				if v, ok := annotations[trackingAnnotationKey]; ok && match(v) {
					logger.WithFields(logrus.Fields{
						"deployment":  dep.GetNamespace() + "/" + dep.GetName(),
						"tracking-id": v,
					}).Info("Found existing matching deployment")
					k.ownerCacheMu.Lock()
					k.topLevelUIDs[string(dep.GetUID())] = struct{}{}
					k.ownerCacheMu.Unlock()
					foundWorkloads = append(foundWorkloads, dep)
				}
			}
		}
	}

	// Проверяем StatefulSets
	statefulsets, err := factory.Apps().V1().StatefulSets().Lister().StatefulSets(k.Namespace).List(labels.Everything())
	if err != nil {
		logger.WithError(err).Warning("Failed to list existing statefulsets")
	} else {
		for _, sts := range statefulsets {
			if annotations := sts.GetAnnotations(); annotations != nil {
				if v, ok := annotations[trackingAnnotationKey]; ok && match(v) {
					logger.WithFields(logrus.Fields{
						"statefulset": sts.GetNamespace() + "/" + sts.GetName(),
						"tracking-id": v,
					}).Info("Found existing matching statefulset")
					k.ownerCacheMu.Lock()
					k.topLevelUIDs[string(sts.GetUID())] = struct{}{}
					k.ownerCacheMu.Unlock()
					foundWorkloads = append(foundWorkloads, sts)
				}
			}
		}
	}

	// Проверяем DaemonSets
	daemonsets, err := factory.Apps().V1().DaemonSets().Lister().DaemonSets(k.Namespace).List(labels.Everything())
	if err != nil {
		logger.WithError(err).Warning("Failed to list existing daemonsets")
	} else {
		for _, ds := range daemonsets {
			if annotations := ds.GetAnnotations(); annotations != nil {
				if v, ok := annotations[trackingAnnotationKey]; ok && match(v) {
					logger.WithFields(logrus.Fields{
						"daemonset":   ds.GetNamespace() + "/" + ds.GetName(),
						"tracking-id": v,
					}).Info("Found existing matching daemonset")
					k.ownerCacheMu.Lock()
					k.topLevelUIDs[string(ds.GetUID())] = struct{}{}
					k.ownerCacheMu.Unlock()
					foundWorkloads = append(foundWorkloads, ds)
				}
			}
		}
	}

	// Проверяем Jobs
	jobs, err := factory.Batch().V1().Jobs().Lister().Jobs(k.Namespace).List(labels.Everything())
	if err != nil {
		logger.WithError(err).Warning("Failed to list existing jobs")
	} else {
		for _, job := range jobs {
			if annotations := job.GetAnnotations(); annotations != nil {
				if v, ok := annotations[trackingAnnotationKey]; ok && match(v) {
					logger.WithFields(logrus.Fields{
						"job":         job.GetNamespace() + "/" + job.GetName(),
						"tracking-id": v,
					}).Info("Found existing matching job")
					k.ownerCacheMu.Lock()
					k.topLevelUIDs[string(job.GetUID())] = struct{}{}
					k.ownerCacheMu.Unlock()
					foundWorkloads = append(foundWorkloads, job)
				}
			}
		}
	}

	// Проверяем CronJobs
	cronjobs, err := factory.Batch().V1().CronJobs().Lister().CronJobs(k.Namespace).List(labels.Everything())
	if err != nil {
		logger.WithError(err).Warning("Failed to list existing cronjobs")
	} else {
		for _, cj := range cronjobs {
			if annotations := cj.GetAnnotations(); annotations != nil {
				if v, ok := annotations[trackingAnnotationKey]; ok && match(v) {
					logger.WithFields(logrus.Fields{
						"cronjob":     cj.GetNamespace() + "/" + cj.GetName(),
						"tracking-id": v,
					}).Info("Found existing matching cronjob")
					k.ownerCacheMu.Lock()
					k.topLevelUIDs[string(cj.GetUID())] = struct{}{}
					k.ownerCacheMu.Unlock()
					foundWorkloads = append(foundWorkloads, cj)
				}
			}
		}
	}

	logger.WithField("count", len(foundWorkloads)).Info("Found existing matching workloads")

	// Теперь найдем поды для этих workload'ов
	if len(foundWorkloads) > 0 {
		k.scanPodsForWorkloads(ctx)
	}
}

// scanPodsForWorkloads - находит поды принадлежащие найденным workload'ам
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

// onPodEvent — обёртка
func (k *KubeLogStreamer) onPodEvent(ctx context.Context, obj interface{}) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		logger.WithField("type", fmt.Sprintf("%T", obj)).Warning("Received non-pod object in pod event handler")
		return
	}
	logger.WithField("pod", pod.Name).Debug("Pod event received")
	k.onPod(ctx, pod)
}

// onPod — фильтрует pod по владельцам и стартует логи для всех контейнеров
func (k *KubeLogStreamer) onPod(ctx context.Context, pod *corev1.Pod) {
	k.onPodWithExistingFlag(ctx, pod, false)
}

// onPodWithExistingFlag — внутренняя функция для обработки подов с флагом существующего пода
func (k *KubeLogStreamer) onPodWithExistingFlag(ctx context.Context, pod *corev1.Pod, isExistingPod bool) {
	if pod.Namespace != k.Namespace {
		logger.WithFields(logrus.Fields{
			"pod":                pod.Name,
			"expected_namespace": k.Namespace,
		}).Debug("Skipping pod: wrong namespace")
		return
	}

	// Проверяем принадлежность к нашим top-level ворклоадам
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
	// обычные контейнеры
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

// поднимаемся по ownerReferences до top-level и проверяем вхождение в k.topLevelUIDs
func (k *KubeLogStreamer) podBelongsToTargets(ctx context.Context, pod *corev1.Pod) bool {
	logger.WithField("pod", pod.Name).Debug("Checking ownership chain")

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
			rs, err := k.Client.AppsV1().ReplicaSets(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
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
			ss, err := k.Client.AppsV1().StatefulSets(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
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
			ds, err := k.Client.AppsV1().DaemonSets(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
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
			job, err := k.Client.BatchV1().Jobs(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
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
			dep, err := k.Client.AppsV1().Deployments(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
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
			cj, err := k.Client.BatchV1().CronJobs(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
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

// makeTrackingMatcher — точное совпадение или "содержит" namespace и имя приложения
func (k *KubeLogStreamer) makeTrackingMatcher() func(string) bool {
	appName := k.TrackingID
	ns := k.Namespace

	if k.TrackingIDExact {
		want := fmt.Sprintf("%s_%s:apps/Deployment:%s/%s", ns, appName, ns, appName)
		logger.WithField("tracking_id", want).Debug("Using exact matching for tracking ID")
		return func(got string) bool {
			match := got == want
			if match {
				logger.WithField("tracking_id", got).Debug("Exact match found")
			}
			return match
		}
	}

	// Формируем ожидаемые части tracking-id в формате: namespace_app-name:apps/Kind:namespace/app-name
	expectedPrefix := fmt.Sprintf("%s_%s:", ns, appName)
	expectedSuffix := fmt.Sprintf(":%s/%s", ns, appName)

	logger.WithFields(logrus.Fields{
		"app_name":        appName,
		"namespace":       ns,
		"expected_prefix": expectedPrefix,
		"expected_suffix": expectedSuffix,
	}).Debug("Using partial matching for tracking ID")

	return func(got string) bool {
		// Проверяем что tracking-id содержит префикс namespace_app-name: и суффикс :namespace/app-name
		if !strings.HasPrefix(got, expectedPrefix) {
			logger.WithFields(logrus.Fields{
				"tracking_id":     got,
				"expected_prefix": expectedPrefix,
			}).Debug("Tracking ID does not have expected prefix")
			return false
		}
		if !strings.HasSuffix(got, expectedSuffix) {
			logger.WithFields(logrus.Fields{
				"tracking_id":     got,
				"expected_suffix": expectedSuffix,
			}).Debug("Tracking ID does not have expected suffix")
			return false
		}
		logger.WithFields(logrus.Fields{
			"tracking_id":     got,
			"expected_prefix": expectedPrefix,
			"expected_suffix": expectedSuffix,
		}).Debug("Tracking ID matches expected pattern")
		return true
	}
}
