// workloads/deployment.go
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

const trackingAnnotationKey = "argocd.argoproj.io/tracking-id"

// CreateDeploymentMatcher creates a matcher function for Deployment tracking IDs
func CreateDeploymentMatcher(namespace, appName string, exact bool) func(string) bool {
	if exact {
		want := fmt.Sprintf("%s_%s:apps/Deployment:%s/%s", namespace, appName, namespace, appName)
		logger.WithField("deployment_tracking_id", want).Debug("Using exact matching for Deployment tracking ID")
		return func(got string) bool {
			match := got == want
			if match {
				logger.WithField("deployment_tracking_id", got).Debug("Exact Deployment match found")
			}
			return match
		}
	}

	// Partial matching for Deployment
	expectedPrefix := fmt.Sprintf("%s_%s:", namespace, appName)
	expectedSuffix := fmt.Sprintf(":apps/Deployment:%s/%s", namespace, appName)

	logger.WithFields(logrus.Fields{
		"app_name":        appName,
		"namespace":       namespace,
		"expected_prefix": expectedPrefix,
		"expected_suffix": expectedSuffix,
	}).Debug("Using partial matching for Deployment tracking ID")

	return func(got string) bool {
		if !strings.HasPrefix(got, expectedPrefix) {
			return false
		}
		if !strings.HasSuffix(got, expectedSuffix) {
			return false
		}
		logger.WithFields(logrus.Fields{
			"deployment_tracking_id": got,
			"expected_prefix":        expectedPrefix,
			"expected_suffix":        expectedSuffix,
		}).Debug("Deployment tracking ID matches expected pattern")
		return true
	}
}

// SetupDeploymentWatcher sets up the Deployment informer and event handlers
func SetupDeploymentWatcher(factory informers.SharedInformerFactory, namespace, appName string, exact bool, addIfMatch func(metav1.Object) bool, delUID func(metav1.Object), rescan func()) {
	logger.Debug("Setting up Deployment watcher")
	informer := factory.Apps().V1().Deployments().Informer()
	matcher := CreateDeploymentMatcher(namespace, appName, exact)

	addIfMatchDeployment := func(obj metav1.Object) bool {
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
				if addIfMatchDeployment(o) {
					logger.WithField("deployment", o.GetNamespace()+"/"+o.GetName()).Debug("Deployment added")
					rescan()
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if o, ok := newObj.(metav1.Object); ok {
				if addIfMatchDeployment(o) {
					logger.WithField("deployment", o.GetNamespace()+"/"+o.GetName()).Debug("Deployment updated")
					rescan()
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				if addIfMatchDeployment(o) {
					logger.WithField("deployment", o.GetNamespace()+"/"+o.GetName()).Debug("Deployment deleted")
					delUID(o)
				}
			}
		},
	})
}

// ScanExistingDeployments scans for existing deployments with matching tracking IDs
func ScanExistingDeployments(factory informers.SharedInformerFactory, namespace, appName string, exact bool, addToCache func(string)) []metav1.Object {
	var foundWorkloads []metav1.Object
	matcher := CreateDeploymentMatcher(namespace, appName, exact)

	deployments, err := factory.Apps().V1().Deployments().Lister().Deployments(namespace).List(labels.Everything())
	if err != nil {
		logger.WithError(err).Warning("Failed to list existing deployments")
		return foundWorkloads
	}

	for _, dep := range deployments {
		if annotations := dep.GetAnnotations(); annotations != nil {
			if v, ok := annotations[trackingAnnotationKey]; ok && matcher(v) {
				logger.WithFields(logrus.Fields{
					"deployment":  dep.GetNamespace() + "/" + dep.GetName(),
					"tracking-id": v,
				}).Info("Found existing matching deployment")
				addToCache(string(dep.GetUID()))
				foundWorkloads = append(foundWorkloads, dep)
			}
		}
	}

	return foundWorkloads
}
