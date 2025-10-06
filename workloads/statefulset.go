// workloads/statefulset.go
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

// CreateStatefulSetMatcher creates a matcher function for StatefulSet tracking IDs
func CreateStatefulSetMatcher(namespace, appName string, exact bool) func(string) bool {
	if exact {
		want := fmt.Sprintf("%s_%s:apps/StatefulSet:%s/%s", namespace, appName, namespace, appName)
		logger.WithField("statefulset_tracking_id", want).Debug("Using exact matching for StatefulSet tracking ID")
		return func(got string) bool {
			match := got == want
			if match {
				logger.WithField("statefulset_tracking_id", got).Debug("Exact StatefulSet match found")
			}
			return match
		}
	}

	// Partial matching for StatefulSet
	expectedPrefix := fmt.Sprintf("%s_%s:", namespace, appName)
	expectedSuffix := fmt.Sprintf(":apps/StatefulSet:%s/%s", namespace, appName)

	logger.WithFields(logrus.Fields{
		"app_name":        appName,
		"namespace":       namespace,
		"expected_prefix": expectedPrefix,
		"expected_suffix": expectedSuffix,
	}).Debug("Using partial matching for StatefulSet tracking ID")

	return func(got string) bool {
		if !strings.HasPrefix(got, expectedPrefix) {
			return false
		}
		if !strings.HasSuffix(got, expectedSuffix) {
			return false
		}
		logger.WithFields(logrus.Fields{
			"statefulset_tracking_id": got,
			"expected_prefix":         expectedPrefix,
			"expected_suffix":         expectedSuffix,
		}).Debug("StatefulSet tracking ID matches expected pattern")
		return true
	}
}

// SetupStatefulSetWatcher sets up the StatefulSet informer and event handlers
func SetupStatefulSetWatcher(factory informers.SharedInformerFactory, namespace, appName string, exact bool, addIfMatch func(metav1.Object) bool, delUID func(metav1.Object), rescan func()) {
	logger.Debug("Setting up StatefulSet watcher")
	informer := factory.Apps().V1().StatefulSets().Informer()
	matcher := CreateStatefulSetMatcher(namespace, appName, exact)

	addIfMatchStatefulSet := func(obj metav1.Object) bool {
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
				if addIfMatchStatefulSet(o) {
					logger.WithField("statefulset", o.GetNamespace()+"/"+o.GetName()).Debug("StatefulSet added")
					rescan()
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if o, ok := newObj.(metav1.Object); ok {
				if addIfMatchStatefulSet(o) {
					logger.WithField("statefulset", o.GetNamespace()+"/"+o.GetName()).Debug("StatefulSet updated")
					rescan()
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				if addIfMatchStatefulSet(o) {
					logger.WithField("statefulset", o.GetNamespace()+"/"+o.GetName()).Debug("StatefulSet deleted")
					delUID(o)
				}
			}
		},
	})
}

// ScanExistingStatefulSets scans for existing statefulsets with matching tracking IDs
func ScanExistingStatefulSets(factory informers.SharedInformerFactory, namespace, appName string, exact bool, addToCache func(string)) []metav1.Object {
	var foundWorkloads []metav1.Object
	matcher := CreateStatefulSetMatcher(namespace, appName, exact)

	statefulsets, err := factory.Apps().V1().StatefulSets().Lister().StatefulSets(namespace).List(labels.Everything())
	if err != nil {
		logger.WithError(err).Warning("Failed to list existing statefulsets")
		return foundWorkloads
	}

	for _, sts := range statefulsets {
		if annotations := sts.GetAnnotations(); annotations != nil {
			if v, ok := annotations[trackingAnnotationKey]; ok && matcher(v) {
				logger.WithFields(logrus.Fields{
					"statefulset": sts.GetNamespace() + "/" + sts.GetName(),
					"tracking-id": v,
				}).Info("Found existing matching statefulset")
				addToCache(string(sts.GetUID()))
				foundWorkloads = append(foundWorkloads, sts)
			}
		}
	}

	return foundWorkloads
}
