// logging.go
package main

import (
	"context"
	"fmt"
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

	logger.WithField("level", logger.GetLevel().String()).Info("Logger initialized")
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
	logger.WithField("stream", streamKey).Debug("Starting log stream")

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
		fmt.Fprintf(ls.out, "[kubectl] logs --namespace=%s pod/%s -c %s --tail=10 (follow)\n", ls.namespace, podName, containerName)
	} else {
		fmt.Fprintf(ls.out, "[kubectl] logs --namespace=%s pod/%s -c %s (follow)\n", ls.namespace, podName, containerName)
	}

	logger.WithField("stream", streamKey).Debug("Creating log stream request")
	req := ls.client.CoreV1().Pods(ls.namespace).GetLogs(podName, logOptions)
	stream, err := req.Stream(ctx)
	if err != nil {
		logger.WithField("stream", streamKey).WithError(err).Error("Failed to create log stream")
		return err
	}
	defer stream.Close()

	logger.WithField("stream", streamKey).Info("Log stream established, copying logs")
	prefix := fmt.Sprintf("[%s %s] ", podName, containerName)
	if err := copyWithPrefix(ctx, ls.out, stream, prefix); err != nil && err != context.Canceled {
		logger.WithField("stream", streamKey).WithError(err).Warning("Error copying logs")
		fmt.Fprintf(ls.out, "[warn] copy logs error pod=%s container=%s: %v\n", podName, containerName, err)
		return err
	} else {
		logger.WithField("stream", streamKey).Info("Log stream ended normally")
	}
	return nil
}

func (ls *LogStreamer) waitContainerReady(ctx context.Context, podName, container string) {
	streamKey := podName + "/" + container
	logger.WithField("stream", streamKey).Debug("Waiting for container to be ready")

	t := time.NewTicker(700 * time.Millisecond)
	defer t.Stop()

	checkCount := 0
	for {
		select {
		case <-ctx.Done():
			logger.WithField("stream", streamKey).Debug("Context cancelled while waiting for container")
			return
		case <-t.C:
			checkCount++
			pod, err := ls.client.CoreV1().Pods(ls.namespace).Get(ctx, podName, metav1.GetOptions{})
			if err != nil {
				logger.WithField("stream", streamKey).WithError(err).Warning("Failed to get pod while waiting for container")
				return
			}

			for _, cs := range append(pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses...) {
				if cs.Name == container {
					if cs.ContainerID != "" {
						logger.WithFields(logrus.Fields{
							"stream":       streamKey,
							"container_id": cs.ContainerID,
						}).Debug("Container is ready")
						return
					}
					if checkCount%10 == 0 { // Log every 7 seconds
						logger.WithFields(logrus.Fields{
							"stream": streamKey,
							"state":  fmt.Sprintf("%+v", cs.State),
						}).Debug("Container still not ready")
					}
					break
				}
			}
		}
	}
}

// copyWithPrefix copies from src to dst, adding prefix to each line
func copyWithPrefix(ctx context.Context, dst io.Writer, src io.Reader, prefix string) error {
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
				line = append(line, chunk[:i+1]...)
				if _, werr := io.WriteString(dst, prefix+string(line)); werr != nil {
					return werr
				}
				line = line[:0]
				chunk = chunk[i+1:]
			}
		}
		if err != nil {
			if err == io.EOF {
				if len(line) > 0 {
					_, _ = io.WriteString(dst, prefix+string(line))
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

// FormatKubectlCommand formats kubectl command output string
func FormatKubectlCommand(namespace, podName, containerName string, hasTail bool) string {
	if hasTail {
		return fmt.Sprintf("[kubectl] logs --namespace=%s pod/%s -c %s --tail=10 (follow)\n", namespace, podName, containerName)
	}
	return fmt.Sprintf("[kubectl] logs --namespace=%s pod/%s -c %s (follow)\n", namespace, podName, containerName)
}

// FormatLogPrefix formats log line prefix
func FormatLogPrefix(podName, containerName string) string {
	return fmt.Sprintf("[%s %s] ", podName, containerName)
}

// FormatErrorMessage formats error message for log output
func FormatErrorMessage(podName, containerName string, err error) string {
	return fmt.Sprintf("[warn] copy logs error pod=%s container=%s: %v\n", podName, containerName, err)
}
