// workloads/cronjob.go
package workloads

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

type CronJobHandler struct{}

func NewCronJobHandler() *CronJobHandler {
	return &CronJobHandler{}
}

func (c *CronJobHandler) GetTypeName() string {
	return "cronjob"
}

func (c *CronJobHandler) SetupInformer(factory informers.SharedInformerFactory, handlers WorkloadEventHandlers) cache.SharedIndexInformer {
	logger.Debug("Setting up CronJob watcher")
	informer := factory.Batch().V1().CronJobs().Informer()

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("cronjob", o.GetNamespace()+"/"+o.GetName()).Debug("CronJob added")
				if handlers.AddIfMatch(o) {
					handlers.Rescan()
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if o, ok := newObj.(metav1.Object); ok {
				logger.WithField("cronjob", o.GetNamespace()+"/"+o.GetName()).Debug("CronJob updated")
				if handlers.AddIfMatch(o) {
					handlers.Rescan()
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("cronjob", o.GetNamespace()+"/"+o.GetName()).Debug("CronJob deleted")
				handlers.DelUID(o)
			}
		},
	})

	return informer
}

func (c *CronJobHandler) ListExisting(factory informers.SharedInformerFactory, namespace string) ([]metav1.Object, error) {
	cronjobs, err := factory.Batch().V1().CronJobs().Lister().CronJobs(namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	var objects []metav1.Object
	for _, cj := range cronjobs {
		objects = append(objects, cj)
	}
	return objects, nil
}
