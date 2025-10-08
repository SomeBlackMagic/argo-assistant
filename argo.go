package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

// getOutputPrefix determines the prefix for an output line based on its content
func getOutputPrefix(line string) string {
	// Check for Kubernetes events and logs
	//kubeIndicators := []string{
	//	"Event:",
	//	"Events:",
	//	"kubectl",
	//	"pod/",
	//	"deployment/",
	//	"service/",
	//	"configmap/",
	//	"secret/",
	//	"namespace/",
	//	"Warning",
	//	"Normal",
	//	"FailedMount",
	//	"Scheduled",
	//	"Pulled",
	//	"Created",
	//	"Started",
	//	"Killing",
	//	"logs",
	//	"NAMESPACE",
	//	"NAME",
	//	"READY",
	//	"STATUS",
	//	"RESTARTS",
	//	"AGE",
	//}
	//
	//lineLower := strings.ToLower(line)
	//for _, indicator := range kubeIndicators {
	//	if strings.Contains(lineLower, strings.ToLower(indicator)) {
	//		return "[kube]"
	//	}
	//}

	// By default, consider everything else as ArgoCD output
	return "[argo]"
}

// RunArgoWait runs `argocd app wait <app> --app-namespace=<ns>`,
// passes stdout/stderr through and returns subprocess exit code.
// Only environment variables with "ARGO" prefix are passed to subprocess.
func RunArgoWait(ctx context.Context, argocdBin, appName, namespace string) int {
	args := []string{"app", "wait", appName, "--app-namespace=" + namespace, "--health", "--sync", "--grpc-web"}
	logger.WithFields(logrus.Fields{
		"command":   argocdBin,
		"args":      strings.Join(args, " "),
		"app":       appName,
		"namespace": namespace,
	}).Info("Executing argocd command")

	cmd := exec.CommandContext(ctx, argocdBin, args...)

	// Get pipes for stdout and stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		logger.WithError(err).Error("Error creating stdout pipe")
		return 1
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		logger.WithError(err).Error("Error creating stderr pipe")
		return 1
	}

	// Pass environment: only ARGO_*
	var argoEnv []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ARGO") {
			argoEnv = append(argoEnv, kv)
		}
		if strings.Contains(kv, "HOME") {
			argoEnv = append(argoEnv, kv)
		}

	}
	cmd.Env = argoEnv

	// Start the command
	if err := cmd.Start(); err != nil {
		logger.WithError(err).Error("Error starting command")
		return 1
	}

	var wg sync.WaitGroup

	// Process stdout
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			line := scanner.Text()
			prefix := getOutputPrefix(line)
			fmt.Printf("%s %s\n", prefix, line)
		}
	}()

	// Process stderr
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			prefix := getOutputPrefix(line)
			logger.WithFields(logrus.Fields{
				"prefix": prefix,
				"source": "stderr",
			}).Warn(line)
		}
	}()

	// Wait for all streams to complete processing
	wg.Wait()

	err = cmd.Wait()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		logger.WithFields(logrus.Fields{
			"exit_code": ee.ExitCode(),
		}).Error("Argocd command exited with error")
		return ee.ExitCode()
	}
	logger.WithError(err).Error("Error running argocd command")
	return 1
}
