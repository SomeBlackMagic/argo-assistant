// logging.go
package main

import (
	"context"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var logger = logrus.New()

func init() {
	// Configure logrus
	logger.SetFormatter(&logrus.TextFormatter{
		FullTimestamp:   true,
		TimestampFormat: "2006-01-02 15:04:05",
	})

	// Set log level from environment variable or default to Info
	level := os.Getenv("LOG_LEVEL")
	switch strings.ToLower(level) {
	case "debug":
		logger.SetLevel(logrus.DebugLevel)
	case "info":
		logger.SetLevel(logrus.InfoLevel)
	case "warning", "warn":
		logger.SetLevel(logrus.WarnLevel)
	case "error":
		logger.SetLevel(logrus.ErrorLevel)
	default:
		logger.SetLevel(logrus.InfoLevel)
	}
}

// LogStreamer handles the actual streaming and formatting of container logs
type LogStreamer struct {
	client    *kubernetes.Clientset
	namespace string
	out       io.Writer
}

func NewLogStreamer(client *kubernetes.Clientset, namespace string, out io.Writer) *LogStreamer {
	return &LogStreamer{
		client:    client,
		namespace: namespace,
		out:       out,
	}
}

// StartLogStream starts streaming logs for a specific pod/container
func (ls *LogStreamer) StartLogStream(ctx context.Context, podName, containerName string, isExistingPod bool) error {
	streamKey := podName + "/" + containerName

	// Wait for container to be ready
	ls.waitContainerReady(ctx, podName, containerName)

	logOptions := &corev1.PodLogOptions{
		Container:  containerName,
		Follow:     true,
		Timestamps: true,
	}

	// For existing pods show only last 10 lines
	if isExistingPod {
		tailLines := int64(10)
		logOptions.TailLines = &tailLines
		logger.WithFields(logrus.Fields{
			"namespace": ls.namespace,
			"pod":       podName,
			"container": containerName,
			"tail":      10,
		}).Debug("Starting log stream for existing pod with tail")
	} else {
		logger.WithFields(logrus.Fields{
			"namespace": ls.namespace,
			"pod":       podName,
			"container": containerName,
		}).Debug("Starting log stream for new pod")
	}

	req := ls.client.CoreV1().Pods(ls.namespace).GetLogs(podName, logOptions)
	stream, err := req.Stream(ctx)
	if err != nil {
		logger.WithField("stream", streamKey).WithError(err).Error("Failed to create log stream")
		return err
	}
	defer stream.Close()

	if err := copyWithLogrus(ctx, stream, podName, containerName); err != nil && err != context.Canceled {
		logger.WithFields(logrus.Fields{
			"stream":    streamKey,
			"pod":       podName,
			"container": containerName,
		}).WithError(err).Warning("Error copying logs")
		return err
	}
	return nil
}

func (ls *LogStreamer) waitContainerReady(ctx context.Context, podName, container string) {
	streamKey := podName + "/" + container

	t := time.NewTicker(700 * time.Millisecond)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			pod, err := ls.client.CoreV1().Pods(ls.namespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				logger.WithField("stream", streamKey).WithError(err).Warning("Failed to get pod while waiting for container")
				return
			}

			for _, cs := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
				if cs.Name == container {
					if cs.ContainerID != "" {
						return
					}
					break
				}
			}
		}
	}
}

// copyWithLogrus copies from src to logrus logger, streaming logs line by line
func copyWithLogrus(ctx context.Context, src io.Reader, podName, containerName string) error {
	buf := make([]byte, 32*1024)
	var line []byte
	for {
		n, err := src.Read(buf)
		if n > 0 {
			chunk := buf[:n]
			for {
				i := indexByte(chunk, '\n')
				if i < 0 {
					line = append(line, chunk...)
					break
				}
				line = append(line, chunk[:i]...)

				// Log through logrus
				logLine := string(line)
				logger.WithFields(logrus.Fields{
					"pod":       podName,
					"container": containerName,
					"source":    "pod_log",
				}).Info(logLine)

				line = line[:0]
				chunk = chunk[i+1:]
			}
		}
		if err != nil {
			if err == io.EOF {
				if len(line) > 0 {
					logLine := string(line)
					logger.WithFields(logrus.Fields{
						"pod":       podName,
						"container": containerName,
						"source":    "pod_log",
					}).Info(logLine)
				}
				return nil
			}
			return err
		}
	}
}

// indexByte returns the index of byte c in slice b, or -1 if not found
func indexByte(b []byte, c byte) int {
	for i, v := range b {
		if v == c {
			return i
		}
	}
	return -1
}
