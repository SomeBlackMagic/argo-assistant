// workloads/statefulset.go
package workloads

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

type StatefulSetHandler struct{}

func NewStatefulSetHandler() *StatefulSetHandler {
	return &StatefulSetHandler{}
}

func (s *StatefulSetHandler) GetTypeName() string {
	return "statefulset"
}

func (s *StatefulSetHandler) SetupInformer(factory informers.SharedInformerFactory, handlers WorkloadEventHandlers) cache.SharedIndexInformer {
	logger.Debug("Setting up StatefulSet watcher")
	informer := factory.Apps().V1().StatefulSets().Informer()

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("statefulset", o.GetNamespace()+"/"+o.GetName()).Debug("StatefulSet added")
				if handlers.AddIfMatch(o) {
					handlers.Rescan()
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if o, ok := newObj.(metav1.Object); ok {
				logger.WithField("statefulset", o.GetNamespace()+"/"+o.GetName()).Debug("StatefulSet updated")
				if handlers.AddIfMatch(o) {
					handlers.Rescan()
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("statefulset", o.GetNamespace()+"/"+o.GetName()).Debug("StatefulSet deleted")
				handlers.DelUID(o)
			}
		},
	})

	return informer
}

func (s *StatefulSetHandler) ListExisting(factory informers.SharedInformerFactory, namespace string) ([]metav1.Object, error) {
	statefulsets, err := factory.Apps().V1().StatefulSets().Lister().StatefulSets(namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	var objects []metav1.Object
	for _, sts := range statefulsets {
		objects = append(objects, sts)
	}
	return objects, nil
}
