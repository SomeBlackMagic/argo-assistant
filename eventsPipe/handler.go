package eventsPipe

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// EventHandler handles Kubernetes events and outputs them with appropriate log levels
type EventHandler struct {
	client    *kubernetes.Clientset
	namespace string
	logger    *logrus.Logger

	// track last seen event time per pod
	eventTimestampMu sync.Mutex
	lastEventTime    map[string]time.Time // podName -> last event timestamp
}

// NewEventHandler creates a new event handler
func NewEventHandler(client *kubernetes.Clientset, namespace string, logger *logrus.Logger) *EventHandler {
	return &EventHandler{
		client:        client,
		namespace:     namespace,
		logger:        logger,
		lastEventTime: make(map[string]time.Time),
	}
}

// PodOwnershipChecker is an interface for checking if a pod belongs to tracked workloads
type PodOwnershipChecker interface {
	PodBelongsToTargets(ctx context.Context, pod *corev1.Pod) bool
}

// HandleEvent processes a Kubernetes event and logs it if it belongs to tracked workloads
func (h *EventHandler) HandleEvent(ctx context.Context, obj interface{}, ownerChecker PodOwnershipChecker) {
	event, ok := obj.(*corev1.Event)
	if !ok {
		h.logger.WithField("type", fmt.Sprintf("%T", obj)).Warning("Received non-event object in event handler")
		return
	}

	// Only process events related to pods
	if event.InvolvedObject.Kind != "Pod" {
		return
	}

	// Get the pod name from the event
	podName := event.InvolvedObject.Name

	// Check if we have already seen this exact event (based on timestamp and message)
	eventKey := podName
	h.eventTimestampMu.Lock()
	lastTime, exists := h.lastEventTime[eventKey]

	// Skip if we've seen a newer or equal event for this pod
	if exists && !event.LastTimestamp.Time.After(lastTime) {
		h.eventTimestampMu.Unlock()
		return
	}

	h.lastEventTime[eventKey] = event.LastTimestamp.Time
	h.eventTimestampMu.Unlock()

	// Check if the pod belongs to our tracked workloads
	// We need to fetch the pod to check ownership
	pod, err := h.client.CoreV1().Pods(h.namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		// Pod might have been deleted, but we still want to show the event
		h.logger.WithField("pod", podName).WithError(err).Debug("Could not fetch pod for event, skipping ownership check")
		return
	}

	if !ownerChecker.PodBelongsToTargets(ctx, pod) {
		return
	}

	// Format and output the event
	h.LogEvent(event, podName)
}

// LogEvent formats and logs a Kubernetes event with appropriate log level
func (h *EventHandler) LogEvent(event *corev1.Event, podName string) {
	timestamp := event.LastTimestamp.Time.Format("2006-01-02T15:04:05Z")
	eventType := event.Type
	reason := event.Reason
	message := event.Message
	count := event.Count

	// Map event to log level
	logLevel := MapEventToLogLevel(event)

	// Log with appropriate level
	logEntry := h.logger.WithFields(logrus.Fields{
		"pod":       podName,
		"timestamp": timestamp,
		"type":      eventType,
		"reason":    reason,
		"count":     count,
		"source":    "event",
	})

	switch logLevel {
	case Debug:
		logEntry.Debug(message)
	case Info:
		logEntry.Info(message)
	case Warn:
		logEntry.Warn(message)
	case Error:
		logEntry.Error(message)
	default:
		logEntry.Info(message)
	}
}
