// Package informer provides management for Kubernetes informers
package informer

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
)

// Manager manages Kubernetes informers for pods and events
type Manager struct {
	client    *kubernetes.Clientset
	namespace string
	logger    *logrus.Logger

	factory       informers.SharedInformerFactory
	podInformer   cache.SharedIndexInformer
	eventInformer cache.SharedIndexInformer

	stopCh chan struct{}
}

// PodListerFunc is a function that lists pods
type PodListerFunc func() ([]*corev1.Pod, error)

// NewManager creates a new informer manager
func NewManager(client *kubernetes.Clientset, namespace string, logger *logrus.Logger) *Manager {
	logger.Debug("Creating shared informer factory")
	factory := informers.NewSharedInformerFactoryWithOptions(
		client,
		time.Minute*10,
		informers.WithNamespace(namespace),
	)

	return &Manager{
		client:    client,
		namespace: namespace,
		logger:    logger,
		factory:   factory,
		stopCh:    make(chan struct{}),
	}
}

// Factory returns the shared informer factory
func (m *Manager) Factory() informers.SharedInformerFactory {
	return m.factory
}

// SetupPodInformer sets up the pod informer with event handlers
func (m *Manager) SetupPodInformer(addFunc func(obj interface{}), updateFunc func(oldObj, newObj interface{})) error {
	m.logger.Debug("Setting up pod informer")
	m.podInformer = m.factory.Core().V1().Pods().Informer()
	if m.podInformer == nil {
		m.logger.Error("Failed to create pod informer")
		return fmt.Errorf("failed to create pod informer")
	}

	m.logger.Debug("Adding pod event handlers")
	_, err := m.podInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    addFunc,
		UpdateFunc: updateFunc,
	})
	if err != nil {
		m.logger.WithError(err).Error("Failed to add pod event handler")
		return fmt.Errorf("failed to add pod event handler: %w", err)
	}

	return nil
}

// SetupEventInformer sets up the event informer with event handlers
func (m *Manager) SetupEventInformer(addFunc func(obj interface{}), updateFunc func(oldObj, newObj interface{})) error {
	m.logger.Debug("Setting up event informer")
	m.eventInformer = m.factory.Core().V1().Events().Informer()
	if m.eventInformer == nil {
		m.logger.Error("Failed to create event informer")
		return fmt.Errorf("failed to create event informer")
	}

	m.logger.Debug("Adding event handlers")
	_, err := m.eventInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc:    addFunc,
		UpdateFunc: updateFunc,
	})
	if err != nil {
		m.logger.WithError(err).Error("Failed to add event handler")
		return fmt.Errorf("failed to add event handler: %w", err)
	}

	return nil
}

// CreatePodLister creates a function that lists pods in the namespace
func (m *Manager) CreatePodLister() PodListerFunc {
	m.logger.Debug("Initializing pod lister function")
	lister := m.factory.Core().V1().Pods().Lister()
	if lister == nil {
		m.logger.Error("Failed to create pod lister")
		return nil
	}

	return func() ([]*corev1.Pod, error) {
		return lister.Pods(m.namespace).List(labels.Everything())
	}
}

// Start starts all informers
func (m *Manager) Start() {
	m.logger.Info("Starting informer factory")
	m.factory.Start(m.stopCh)
}

// WaitForCacheSync waits for all caches to sync
func (m *Manager) WaitForCacheSync(ctx context.Context) error {
	m.logger.Info("Waiting for cache sync...")
	if !cache.WaitForCacheSync(ctx.Done(),
		m.podInformer.HasSynced,
		m.eventInformer.HasSynced,
	) {
		m.logger.Error("Cache sync failed")
		return fmt.Errorf("cache not synced")
	}
	m.logger.Info("Cache sync completed successfully")
	return nil
}

// Stop stops all informers
func (m *Manager) Stop() {
	close(m.stopCh)
}
