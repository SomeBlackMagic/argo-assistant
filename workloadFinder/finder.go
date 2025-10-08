// workloadFinder package provides functionality to determine if a pod belongs to tracked workloads
package workloadFinder

import (
	"context"
	"sync"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// WorkloadFinder manages the logic for finding and tracking workload ownership
type WorkloadFinder struct {
	client *kubernetes.Clientset
	logger *logrus.Logger

	ownerCacheMu sync.Mutex
	topLevelUIDs map[string]struct{} // UIDs of top-level workloads (Deployment/StatefulSet/DaemonSet/Job/CronJob)
}

// NewWorkloadFinder creates a new workload finder instance
func NewWorkloadFinder(client *kubernetes.Clientset, logger *logrus.Logger) *WorkloadFinder {
	return &WorkloadFinder{
		client:       client,
		logger:       logger,
		topLevelUIDs: make(map[string]struct{}),
	}
}

// AddTopLevelUID adds a UID to the top-level workload tracking set
func (wf *WorkloadFinder) AddTopLevelUID(uid string) bool {
	wf.ownerCacheMu.Lock()
	defer wf.ownerCacheMu.Unlock()

	_, existed := wf.topLevelUIDs[uid]
	wf.topLevelUIDs[uid] = struct{}{}
	return !existed
}

// RemoveTopLevelUID removes a UID from the top-level workload tracking set
func (wf *WorkloadFinder) RemoveTopLevelUID(uid string) {
	wf.ownerCacheMu.Lock()
	defer wf.ownerCacheMu.Unlock()

	delete(wf.topLevelUIDs, uid)
}

// UIDIsTop checks if a UID belongs to a top-level workload
func (wf *WorkloadFinder) UIDIsTop(uid string) bool {
	wf.ownerCacheMu.Lock()
	defer wf.ownerCacheMu.Unlock()
	_, ok := wf.topLevelUIDs[uid]
	return ok
}

// HasTopOwnerUID checks if any of the owner references belong to top-level workloads
func (wf *WorkloadFinder) HasTopOwnerUID(ors []metav1.OwnerReference) bool {
	wf.ownerCacheMu.Lock()
	defer wf.ownerCacheMu.Unlock()
	for _, or := range ors {
		if _, ok := wf.topLevelUIDs[string(or.UID)]; ok {
			return true
		}
	}
	return false
}

// PodBelongsToTargets traverses ownerReferences up to top-level and checks inclusion in topLevelUIDs
func (wf *WorkloadFinder) PodBelongsToTargets(ctx context.Context, pod *corev1.Pod, namespace string) bool {
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
			wf.logger.WithField("key", key).Warning("Circular reference detected in ownership chain")
			return false
		}
		visited[key] = true

		wf.logger.WithFields(logrus.Fields{
			"kind": kind,
			"name": ns + "/" + name,
			"uid":  uid,
		}).Debug("Checking if resource is a top-level target")

		if wf.UIDIsTop(uid) {
			wf.logger.WithFields(logrus.Fields{
				"kind": kind,
				"name": ns + "/" + name,
			}).Debug("Found matching target workload")
			return true
		}

		var owner *metav1.OwnerReference
		ownerRefs := getOwnerRefs(kind, pod)
		for i := range ownerRefs {
			or := &ownerRefs[i]
			if or.Controller != nil && *or.Controller {
				owner = or
				break
			}
		}
		if owner == nil {
			wf.logger.WithFields(logrus.Fields{
				"kind": kind,
				"name": ns + "/" + name,
			}).Debug("No controller owner found")
			return false
		}

		wf.logger.WithFields(logrus.Fields{
			"owner_kind": owner.Kind,
			"owner_name": owner.Name,
			"owner_uid":  owner.UID,
		}).Debug("Found controller owner")

		switch owner.Kind {
		case "ReplicaSet":
			// Check if context is already cancelled before making API call
			select {
			case <-ctx.Done():
				wf.logger.WithField("replicaset", ns+"/"+owner.Name).Debug("Context cancelled, skipping ReplicaSet lookup")
				return false
			default:
			}

			rs, err := wf.client.AppsV1().ReplicaSets(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				// Check for specific error types
				if ctx.Err() != nil {
					wf.logger.WithFields(logrus.Fields{
						"replicaset":  ns + "/" + owner.Name,
						"context_err": ctx.Err().Error(),
					}).Debug("Context cancelled during ReplicaSet lookup")
					return false
				}
				wf.logger.WithField("replicaset", ns+"/"+owner.Name).WithError(err).Warning("Failed to get ReplicaSet")
				return false
			}
			kind, uid, name = "ReplicaSet", string(rs.UID), rs.Name
			wf.logger.WithField("replicaset", ns+"/"+name).Debug("Moving up ownership chain to ReplicaSet")
			if wf.HasTopOwnerUID(rs.OwnerReferences) {
				wf.logger.WithField("replicaset", ns+"/"+name).Debug("ReplicaSet has top-level owner")
				return true
			}
			if or := firstController(rs.OwnerReferences); or != nil {
				owner = or
			} else {
				wf.logger.WithField("replicaset", ns+"/"+name).Debug("ReplicaSet has no controller owner")
				return false
			}
			continue
		case "StatefulSet":
			// Check if context is already cancelled before making API call
			select {
			case <-ctx.Done():
				wf.logger.WithField("statefulset", ns+"/"+owner.Name).Debug("Context cancelled, skipping StatefulSet lookup")
				return false
			default:
			}

			ss, err := wf.client.AppsV1().StatefulSets(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				// Check for specific error types
				if ctx.Err() != nil {
					wf.logger.WithFields(logrus.Fields{
						"statefulset": ns + "/" + owner.Name,
						"context_err": ctx.Err().Error(),
					}).Debug("Context cancelled during StatefulSet lookup")
					return false
				}
				wf.logger.WithField("statefulset", ns+"/"+owner.Name).WithError(err).Warning("Failed to get StatefulSet")
				return false
			}
			result := wf.UIDIsTop(string(ss.UID))
			wf.logger.WithFields(logrus.Fields{
				"statefulset": ns + "/" + owner.Name,
				"is_target":   result,
			}).Debug("Checked StatefulSet target status")
			return result
		case "DaemonSet":
			// Check if context is already cancelled before making API call
			select {
			case <-ctx.Done():
				wf.logger.WithField("daemonset", ns+"/"+owner.Name).Debug("Context cancelled, skipping DaemonSet lookup")
				return false
			default:
			}

			ds, err := wf.client.AppsV1().DaemonSets(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				// Check for specific error types
				if ctx.Err() != nil {
					wf.logger.WithFields(logrus.Fields{
						"daemonset":   ns + "/" + owner.Name,
						"context_err": ctx.Err().Error(),
					}).Debug("Context cancelled during DaemonSet lookup")
					return false
				}
				wf.logger.WithField("daemonset", ns+"/"+owner.Name).WithError(err).Warning("Failed to get DaemonSet")
				return false
			}
			result := wf.UIDIsTop(string(ds.UID))
			wf.logger.WithFields(logrus.Fields{
				"daemonset": ns + "/" + owner.Name,
				"is_target": result,
			}).Debug("Checked DaemonSet target status")
			return result
		case "Job":
			// Check if context is already cancelled before making API call
			select {
			case <-ctx.Done():
				wf.logger.WithField("job", ns+"/"+owner.Name).Debug("Context cancelled, skipping Job lookup")
				return false
			default:
			}

			job, err := wf.client.BatchV1().Jobs(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				// Check for specific error types
				if ctx.Err() != nil {
					wf.logger.WithFields(logrus.Fields{
						"job":         ns + "/" + owner.Name,
						"context_err": ctx.Err().Error(),
					}).Debug("Context cancelled during Job lookup")
					return false
				}
				wf.logger.WithField("job", ns+"/"+owner.Name).WithError(err).Warning("Failed to get Job")
				return false
			}
			if wf.HasTopOwnerUID(job.OwnerReferences) {
				wf.logger.WithField("job", ns+"/"+owner.Name).Debug("Job has top-level owner")
				return true
			}
			if or := firstController(job.OwnerReferences); or != nil {
				owner = or
				continue
			}
			result := wf.UIDIsTop(string(job.UID))
			wf.logger.WithFields(logrus.Fields{
				"job":       ns + "/" + owner.Name,
				"is_target": result,
			}).Debug("Checked Job target status")
			return result
		case "Deployment":
			// Check if context is already cancelled before making API call
			select {
			case <-ctx.Done():
				wf.logger.WithField("deployment", ns+"/"+owner.Name).Debug("Context cancelled, skipping Deployment lookup")
				return false
			default:
			}

			dep, err := wf.client.AppsV1().Deployments(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				// Check for specific error types
				if ctx.Err() != nil {
					wf.logger.WithFields(logrus.Fields{
						"deployment":  ns + "/" + owner.Name,
						"context_err": ctx.Err().Error(),
					}).Debug("Context cancelled during Deployment lookup")
					return false
				}
				wf.logger.WithField("deployment", ns+"/"+owner.Name).WithError(err).Warning("Failed to get Deployment")
				return false
			}
			result := wf.UIDIsTop(string(dep.UID))
			wf.logger.WithFields(logrus.Fields{
				"deployment": ns + "/" + owner.Name,
				"is_target":  result,
			}).Debug("Checked Deployment target status")
			return result
		case "CronJob":
			// Check if context is already cancelled before making API call
			select {
			case <-ctx.Done():
				wf.logger.WithField("cronjob", ns+"/"+owner.Name).Debug("Context cancelled, skipping CronJob lookup")
				return false
			default:
			}

			cj, err := wf.client.BatchV1().CronJobs(ns).Get(ctx, owner.Name, metav1.GetOptions{})
			if err != nil {
				// Check for specific error types
				if ctx.Err() != nil {
					wf.logger.WithFields(logrus.Fields{
						"cronjob":     ns + "/" + owner.Name,
						"context_err": ctx.Err().Error(),
					}).Debug("Context cancelled during CronJob lookup")
					return false
				}
				wf.logger.WithField("cronjob", ns+"/"+owner.Name).WithError(err).Warning("Failed to get CronJob")
				return false
			}
			result := wf.UIDIsTop(string(cj.UID))
			wf.logger.WithFields(logrus.Fields{
				"cronjob":   ns + "/" + owner.Name,
				"is_target": result,
			}).Debug("Checked CronJob target status")
			return result
		default:
			wf.logger.WithFields(logrus.Fields{
				"owner_kind": owner.Kind,
				"owner_name": ns + "/" + owner.Name,
			}).Warning("Unknown owner kind")
			return false
		}
	}
}

func firstController(ors []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range ors {
		if ors[i].Controller != nil && *ors[i].Controller {
			return &ors[i]
		}
	}
	return nil
}

func getOwnerRefs(kind string, pod *corev1.Pod) []metav1.OwnerReference {
	if kind == "Pod" {
		return pod.OwnerReferences
	}
	return nil
}
