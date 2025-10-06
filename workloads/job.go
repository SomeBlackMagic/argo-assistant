// workloads/job.go
package workloads

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

type JobHandler struct{}

func NewJobHandler() *JobHandler {
	return &JobHandler{}
}

func (j *JobHandler) GetTypeName() string {
	return "job"
}

func (j *JobHandler) SetupInformer(factory informers.SharedInformerFactory, handlers WorkloadEventHandlers) cache.SharedIndexInformer {
	logger.Debug("Setting up Job watcher")
	informer := factory.Batch().V1().Jobs().Informer()

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("job", o.GetNamespace()+"/"+o.GetName()).Debug("Job added")
				if handlers.AddIfMatch(o) {
					handlers.Rescan()
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if o, ok := newObj.(metav1.Object); ok {
				logger.WithField("job", o.GetNamespace()+"/"+o.GetName()).Debug("Job updated")
				if handlers.AddIfMatch(o) {
					handlers.Rescan()
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("job", o.GetNamespace()+"/"+o.GetName()).Debug("Job deleted")
				handlers.DelUID(o)
			}
		},
	})

	return informer
}

func (j *JobHandler) ListExisting(factory informers.SharedInformerFactory, namespace string) ([]metav1.Object, error) {
	jobs, err := factory.Batch().V1().Jobs().Lister().Jobs(namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	var objects []metav1.Object
	for _, job := range jobs {
		objects = append(objects, job)
	}
	return objects, nil
}
