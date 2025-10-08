// workloads/interface.go
package workloads

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
)

// CreateTrackingMatchers creates all tracking matchers for different workload types
func CreateTrackingMatchers(namespace, appName string, exact bool) map[string]func(string) bool {
	return map[string]func(string) bool{
		"Deployment":  CreateDeploymentMatcher(namespace, appName, exact),
		"StatefulSet": CreateStatefulSetMatcher(namespace, appName, exact),
		"DaemonSet":   CreateDaemonSetMatcher(namespace, appName, exact),
		"Job":         CreateJobMatcher(namespace, appName, exact),
		"CronJob":     CreateCronJobMatcher(namespace, appName, exact),
	}
}

// SetupAllWorkloadWatchers sets up all workload watchers
func SetupAllWorkloadWatchers(factory informers.SharedInformerFactory, namespace, appName string, exact bool, addIfMatch func(metav1.Object) bool, delUID func(metav1.Object), rescan func()) {
	SetupDeploymentWatcher(factory, namespace, appName, exact, addIfMatch, delUID, rescan)
	SetupStatefulSetWatcher(factory, namespace, appName, exact, addIfMatch, delUID, rescan)
	//SetupDaemonSetWatcher(factory, namespace, appName, exact, addIfMatch, delUID, rescan)
	//SetupJobWatcher(factory, namespace, appName, exact, addIfMatch, delUID, rescan)
	//SetupCronJobWatcher(factory, namespace, appName, exact, addIfMatch, delUID, rescan)
}

// ScanAllExistingWorkloads scans for all existing workloads
func ScanAllExistingWorkloads(factory informers.SharedInformerFactory, namespace, appName string, exact bool, addToCache func(string)) []metav1.Object {
	var allWorkloads []metav1.Object

	deployments := ScanExistingDeployments(factory, namespace, appName, exact, addToCache)
	allWorkloads = append(allWorkloads, deployments...)

	statefulsets := ScanExistingStatefulSets(factory, namespace, appName, exact, addToCache)
	allWorkloads = append(allWorkloads, statefulsets...)
	//
	//daemonsets := ScanExistingDaemonSets(factory, namespace, appName, exact, addToCache)
	//allWorkloads = append(allWorkloads, daemonsets...)
	//
	//jobs := ScanExistingJobs(factory, namespace, appName, exact, addToCache)
	//allWorkloads = append(allWorkloads, jobs...)
	//
	//cronjobs := ScanExistingCronJobs(factory, namespace, appName, exact, addToCache)
	//allWorkloads = append(allWorkloads, cronjobs...)

	return allWorkloads
}
