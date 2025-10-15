package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

func main() {
	argocd := getEnvStrict("ARGOCD", "argocd")

	if len(os.Args) < 3 {
		logger.WithField("usage", os.Args[0]+" <app-namespace> <app-name>").Error("Invalid arguments")
		os.Exit(1)
	}
	namespace := os.Args[1]
	appName := os.Args[2]

	logger.WithFields(map[string]interface{}{
		"app":       appName,
		"namespace": namespace,
	}).Info("Starting argo-assistant")

	// Context with cancellation and signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// In parallel - log streaming, pass app-name for tracking via tracking-id annotation
	var wg sync.WaitGroup
	logCtx, logCancel := context.WithCancel(ctx)
	wg.Add(1)
	go func() {
		defer wg.Done()
		streamer := NewKubeLogStreamer(namespace, appName, false, os.Stdout)
		if err := streamer.StreamLogsByTrackingID(logCtx); err != nil && err != context.Canceled {
			logger.WithError(err).Error("Log streaming error")
		}
	}()

	// Main subprocess: argocd app wait ...
	exitCode := RunArgoWait(ctx, argocd, appName, namespace)

	// Stop log streamer
	logCancel()
	wg.Wait()

	os.Exit(exitCode)
}

func getEnvStrict(name, defaultVal string) string {
	v := os.Getenv(name)
	if v == "" {
		if defaultVal != "" {
			return defaultVal
		}
		logger.WithField("variable", name).Error("Required environment variable is not set")
		os.Exit(1)
	}
	return v
}
