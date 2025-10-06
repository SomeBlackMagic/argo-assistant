// workloads/daemonset.go
package workloads

import (
	"fmt"
	"strings"

	"github.com/sirupsen/logrus"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// CreateDaemonSetMatcher creates a matcher function for DaemonSet tracking IDs
func CreateDaemonSetMatcher(namespace, appName string, exact bool) func(string) bool {
	if exact {
		want := fmt.Sprintf("%s_%s:apps/DaemonSet:%s/%s", namespace, appName, namespace, appName)
		logger.WithField("daemonset_tracking_id", want).Debug("Using exact matching for DaemonSet tracking ID")
		return func(got string) bool {
			match := got == want
			if match {
				logger.WithField("daemonset_tracking_id", got).Debug("Exact DaemonSet match found")
			}
			return match
		}
	}

	// Partial matching for DaemonSet
	expectedPrefix := fmt.Sprintf("%s_%s:", namespace, appName)
	expectedSuffix := fmt.Sprintf(":apps/DaemonSet:%s/%s", namespace, appName)

	return func(got string) bool {
		if !strings.HasPrefix(got, expectedPrefix) {
			return false
		}
		if !strings.HasSuffix(got, expectedSuffix) {
			return false
		}
		return true
	}
}

// SetupDaemonSetWatcher sets up the DaemonSet informer and event handlers
func SetupDaemonSetWatcher(factory informers.SharedInformerFactory, namespace, appName string, exact bool, addIfMatch func(metav1.Object) bool, delUID func(metav1.Object), rescan func()) {
	logger.Debug("Setting up DaemonSet watcher")
	informer := factory.Apps().V1().DaemonSets().Informer()
	matcher := CreateDaemonSetMatcher(namespace, appName, exact)

	addIfMatchDaemonSet := func(obj metav1.Object) bool {
		annotations := obj.GetAnnotations()
		if annotations == nil {
			return false
		}
		if v, ok := annotations[trackingAnnotationKey]; ok && matcher(v) {
			return addIfMatch(obj)
		}
		return false
	}

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("daemonset", o.GetNamespace()+"/"+o.GetName()).Debug("DaemonSet added")
				if addIfMatchDaemonSet(o) {
					rescan()
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if o, ok := newObj.(metav1.Object); ok {
				logger.WithField("daemonset", o.GetNamespace()+"/"+o.GetName()).Debug("DaemonSet updated")
				if addIfMatchDaemonSet(o) {
					rescan()
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("daemonset", o.GetNamespace()+"/"+o.GetName()).Debug("DaemonSet deleted")
				if addIfMatchDaemonSet(o) {
					delUID(o)
				}
			}
		},
	})
}

// ScanExistingDaemonSets scans for existing daemonsets with matching tracking IDs
func ScanExistingDaemonSets(factory informers.SharedInformerFactory, namespace, appName string, exact bool, addToCache func(string)) []metav1.Object {
	var foundWorkloads []metav1.Object
	matcher := CreateDaemonSetMatcher(namespace, appName, exact)

	daemonsets, err := factory.Apps().V1().DaemonSets().Lister().DaemonSets(namespace).List(labels.Everything())
	if err != nil {
		logger.WithError(err).Warning("Failed to list existing daemonsets")
		return foundWorkloads
	}

	for _, ds := range daemonsets {
		if annotations := ds.GetAnnotations(); annotations != nil {
			if v, ok := annotations[trackingAnnotationKey]; ok && matcher(v) {
				logger.WithFields(logrus.Fields{
					"daemonset":   ds.GetNamespace() + "/" + ds.GetName(),
					"tracking-id": v,
				}).Info("Found existing matching daemonset")
				addToCache(string(ds.GetUID()))
				foundWorkloads = append(foundWorkloads, ds)
			}
		}
	}

	return foundWorkloads
}
