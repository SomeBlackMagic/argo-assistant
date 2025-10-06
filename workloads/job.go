// workloads/job.go
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

// CreateJobMatcher creates a matcher function for Job tracking IDs
func CreateJobMatcher(namespace, appName string, exact bool) func(string) bool {
	if exact {
		want := fmt.Sprintf("%s_%s:batch/Job:%s/%s", namespace, appName, namespace, appName)
		logger.WithField("job_tracking_id", want).Debug("Using exact matching for Job tracking ID")
		return func(got string) bool {
			match := got == want
			if match {
				logger.WithField("job_tracking_id", got).Debug("Exact Job match found")
			}
			return match
		}
	}

	// Partial matching for Job
	expectedPrefix := fmt.Sprintf("%s_%s:", namespace, appName)
	expectedSuffix := fmt.Sprintf(":batch/Job:%s/%s", namespace, appName)

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

// SetupJobWatcher sets up the Job informer and event handlers
func SetupJobWatcher(factory informers.SharedInformerFactory, namespace, appName string, exact bool, addIfMatch func(metav1.Object) bool, delUID func(metav1.Object), rescan func()) {
	logger.Debug("Setting up Job watcher")
	informer := factory.Batch().V1().Jobs().Informer()
	matcher := CreateJobMatcher(namespace, appName, exact)

	addIfMatchJob := func(obj metav1.Object) bool {
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
				if addIfMatchJob(o) {
					logger.WithField("job", o.GetNamespace()+"/"+o.GetName()).Debug("Job added")
					rescan()
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if o, ok := newObj.(metav1.Object); ok {
				if addIfMatchJob(o) {
					logger.WithField("job", o.GetNamespace()+"/"+o.GetName()).Debug("Job updated")
					rescan()
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				if addIfMatchJob(o) {
					logger.WithField("job", o.GetNamespace()+"/"+o.GetName()).Debug("Job deleted")
					delUID(o)
				}
			}
		},
	})
}

// ScanExistingJobs scans for existing jobs with matching tracking IDs
func ScanExistingJobs(factory informers.SharedInformerFactory, namespace, appName string, exact bool, addToCache func(string)) []metav1.Object {
	var foundWorkloads []metav1.Object
	matcher := CreateJobMatcher(namespace, appName, exact)

	jobs, err := factory.Batch().V1().Jobs().Lister().Jobs(namespace).List(labels.Everything())
	if err != nil {
		logger.WithError(err).Warning("Failed to list existing jobs")
		return foundWorkloads
	}

	for _, job := range jobs {
		if annotations := job.GetAnnotations(); annotations != nil {
			if v, ok := annotations[trackingAnnotationKey]; ok && matcher(v) {
				logger.WithFields(logrus.Fields{
					"job":         job.GetNamespace() + "/" + job.GetName(),
					"tracking-id": v,
				}).Info("Found existing matching job")
				addToCache(string(job.GetUID()))
				foundWorkloads = append(foundWorkloads, job)
			}
		}
	}

	return foundWorkloads
}
