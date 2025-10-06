// workloads/deployment.go
package workloads

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/tools/cache"
)

type DeploymentHandler struct{}

func NewDeploymentHandler() *DeploymentHandler {
	return &DeploymentHandler{}
}

func (d *DeploymentHandler) GetTypeName() string {
	return "deployment"
}

func (d *DeploymentHandler) SetupInformer(factory informers.SharedInformerFactory, handlers WorkloadEventHandlers) cache.SharedIndexInformer {
	logger.Debug("Setting up Deployment watcher")
	informer := factory.Apps().V1().Deployments().Informer()

	informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("deployment", o.GetNamespace()+"/"+o.GetName()).Debug("Deployment added")
				if handlers.AddIfMatch(o) {
					handlers.Rescan()
				}
			}
		},
		UpdateFunc: func(_, newObj interface{}) {
			if o, ok := newObj.(metav1.Object); ok {
				logger.WithField("deployment", o.GetNamespace()+"/"+o.GetName()).Debug("Deployment updated")
				if handlers.AddIfMatch(o) {
					handlers.Rescan()
				}
			}
		},
		DeleteFunc: func(obj interface{}) {
			if o, ok := obj.(metav1.Object); ok {
				logger.WithField("deployment", o.GetNamespace()+"/"+o.GetName()).Debug("Deployment deleted")
				handlers.DelUID(o)
			}
		},
	})

	return informer
}

func (d *DeploymentHandler) ListExisting(factory informers.SharedInformerFactory, namespace string) ([]metav1.Object, error) {
	deployments, err := factory.Apps().V1().Deployments().Lister().Deployments(namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}

	var objects []metav1.Object
	for _, dep := range deployments {
		objects = append(objects, dep)
	}
	return objects, nil
}
