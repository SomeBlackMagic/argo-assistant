// Package streaming provides coordination for log streaming
package streaming

import (
	"argocd-watcher/logsPipe"
	"context"
	"sync"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
)

// PodContainerKey uniquely identifies a pod container instance
type PodContainerKey struct {
	PodName       string
	Namespace     string
	ContainerName string
	RestartCount  int32
}

// Coordinator manages active log streams for pods
type Coordinator struct {
	logStreamer *logsPipe.LogStreamer
	logger      *logrus.Logger

	mu            sync.Mutex
	activeStreams map[PodContainerKey]struct{}
}

// NewCoordinator creates a new streaming coordinator
func NewCoordinator(logStreamer *logsPipe.LogStreamer, logger *logrus.Logger) *Coordinator {
	return &Coordinator{
		logStreamer:   logStreamer,
		logger:        logger,
		activeStreams: make(map[PodContainerKey]struct{}),
	}
}

// MaybeStartLogStream starts a log stream for a container if it's not already active
func (c *Coordinator) MaybeStartLogStream(ctx context.Context, pod *corev1.Pod, container string, key PodContainerKey, isExistingPod bool) {
	streamKey := pod.Name + "/" + container

	c.mu.Lock()
	if _, exists := c.activeStreams[key]; exists {
		c.mu.Unlock()
		c.logger.WithFields(logrus.Fields{
			"stream":   streamKey,
			"restarts": key.RestartCount,
		}).Debug("Log stream already active")
		return
	}
	c.activeStreams[key] = struct{}{}
	c.mu.Unlock()

	c.logger.WithFields(logrus.Fields{
		"stream":   streamKey,
		"restarts": key.RestartCount,
	}).Info("Starting new log stream")

	go func() {
		defer func() {
			if r := recover(); r != nil {
				c.logger.WithFields(logrus.Fields{
					"stream": streamKey,
					"panic":  r,
				}).Error("Panic in log stream goroutine")
			}

			c.logger.WithField("stream", streamKey).Debug("Cleaning up log stream")
			c.mu.Lock()
			delete(c.activeStreams, key)
			c.mu.Unlock()
		}()

		// Use LogStreamer to handle the actual log streaming
		if err := c.logStreamer.StartLogStream(ctx, pod.Name, container, isExistingPod); err != nil {
			// Check if the error is due to context cancellation
			if ctx.Err() != nil {
				c.logger.WithFields(logrus.Fields{
					"stream":      streamKey,
					"context_err": ctx.Err().Error(),
				}).Debug("Log stream ended due to context cancellation")
			} else {
				c.logger.WithFields(logrus.Fields{
					"stream": streamKey,
					"error":  err.Error(),
				}).Error("Log stream ended with error")
			}
		} else {
			c.logger.WithField("stream", streamKey).Debug("Log stream ended successfully")
		}
	}()
}
