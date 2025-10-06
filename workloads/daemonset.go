// workloads/daemonset.go
package workloads

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

type DaemonSetHandler struct{}

func NewDaemonSetHandler() *DaemonSetHandler {
	return &DaemonSetHandler{}
}

func (d *DaemonSetHandler) GetTypeName() string {
	return "daemonset"
}

func (d *DaemonSetHandler) SetupInformer(factory informers.SharedInformerFactory, handlers WorkloadEventHandlers) cache.SharedIndexInformer {
	logger.Debug("Setting up DaemonSet watcher")
	informer := factory.Apps().V1().DaemonSets().Informer()

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("daemonset", o.GetNamespace()+"/"+o.GetName()).Debug("DaemonSet added")
				if handlers.AddIfMatch(o) {
					handlers.Rescan()
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if o, ok := newObj.(metav1.Object); ok {
				logger.WithField("daemonset", o.GetNamespace()+"/"+o.GetName()).Debug("DaemonSet updated")
				if handlers.AddIfMatch(o) {
					handlers.Rescan()
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("daemonset", o.GetNamespace()+"/"+o.GetName()).Debug("DaemonSet deleted")
				handlers.DelUID(o)
			}
		},
	})

	return informer
}

func (d *DaemonSetHandler) ListExisting(factory informers.SharedInformerFactory, namespace string) ([]metav1.Object, error) {
	daemonsets, err := factory.Apps().V1().DaemonSets().Lister().DaemonSets(namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	var objects []metav1.Object
	for _, ds := range daemonsets {
		objects = append(objects, ds)
	}
	return objects, nil
}
