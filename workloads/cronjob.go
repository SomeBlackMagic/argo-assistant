// workloads/cronjob.go
package workloads

import (
	"fmt"
	"github.com/sirupsen/logrus"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

// CreateCronJobMatcher creates a matcher function for CronJob tracking IDs
func CreateCronJobMatcher(namespace, appName string, exact bool) func(string) bool {
	if exact {
		want := fmt.Sprintf("%s_%s:batch/CronJob:%s/%s", namespace, appName, namespace, appName)
		logger.WithField("cronjob_tracking_id", want).Debug("Using exact matching for CronJob tracking ID")
		return func(got string) bool {
			match := got == want
			if match {
				logger.WithField("cronjob_tracking_id", got).Debug("Exact CronJob match found")
			}
			return match
		}
	}

	// Partial matching for CronJob
	expectedPrefix := fmt.Sprintf("%s_%s:", namespace, appName)
	expectedSuffix := fmt.Sprintf(":batch/CronJob:%s/%s", namespace, appName)

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

// SetupCronJobWatcher sets up the CronJob informer and event handlers
func SetupCronJobWatcher(factory informers.SharedInformerFactory, namespace, appName string, exact bool, addIfMatch func(metav1.Object) bool, delUID func(metav1.Object), rescan func()) {
	logger.Debug("Setting up CronJob watcher")
	informer := factory.Batch().V1().CronJobs().Informer()
	matcher := CreateCronJobMatcher(namespace, appName, exact)

	addIfMatchCronJob := func(obj metav1.Object) bool {
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
				if addIfMatchCronJob(o) {
					logger.WithField("cronjob", o.GetNamespace()+"/"+o.GetName()).Debug("CronJob added")
					rescan()
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if o, ok := newObj.(metav1.Object); ok {
				if addIfMatchCronJob(o) {
					logger.WithField("cronjob", o.GetNamespace()+"/"+o.GetName()).Debug("CronJob updated")
					rescan()
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				if addIfMatchCronJob(o) {
					logger.WithField("cronjob", o.GetNamespace()+"/"+o.GetName()).Debug("CronJob deleted")
					delUID(o)
				}
			}
		},
	})
}

// ScanExistingCronJobs scans for existing cronjobs with matching tracking IDs
func ScanExistingCronJobs(factory informers.SharedInformerFactory, namespace, appName string, exact bool, addToCache func(string)) []metav1.Object {
	var foundWorkloads []metav1.Object
	matcher := CreateCronJobMatcher(namespace, appName, exact)

	cronjobs, err := factory.Batch().V1().CronJobs().Lister().CronJobs(namespace).List(labels.Everything())
	if err != nil {
		logger.WithError(err).Warning("Failed to list existing cronjobs")
		return foundWorkloads
	}

	for _, cj := range cronjobs {
		if annotations := cj.GetAnnotations(); annotations != nil {
			if v, ok := annotations[trackingAnnotationKey]; ok && matcher(v) {
				logger.WithFields(logrus.Fields{
					"cronjob":     cj.GetNamespace() + "/" + cj.GetName(),
					"tracking-id": v,
				}).Info("Found existing matching cronjob")
				addToCache(string(cj.GetUID()))
				foundWorkloads = append(foundWorkloads, cj)
			}
		}
	}

	return foundWorkloads
}
