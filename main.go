package main

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

var version string = ""
var revision string = "000000000000000000000000000000"

func main() {
	if len(os.Args) < 2 {
		logger.WithField("usage", os.Args[0]+" <command> [args]").Error("Invalid arguments")
		os.Exit(1)
	}

	command := os.Args[1]

	switch command {
	case "version":
		if version == "" {
			version = "dev"
		}
		logger.WithFields(map[string]interface{}{
			"version":  version,
			"revision": revision,
		}).Info("argo-assistant version")
		os.Exit(0)
	case "sync":
		if len(os.Args) < 4 {
			logger.WithField("usage", os.Args[0]+" sync <app-namespace> <app-name>").Error("Invalid arguments")
			os.Exit(1)
		}
		namespace := os.Args[2]
		appName := os.Args[3]
		runSync(namespace, appName)
	default:
		logger.WithField("command", command).Error("Unknown command")
		os.Exit(1)
	}
}

func runSync(namespace, appName string) {
	argocd := getEnvStrict("ARGOCD", "argocd")

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
