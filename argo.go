package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// getOutputPrefix определяет префикс для строки вывода на основе её содержания
func getOutputPrefix(line string) string {
	// Проверяем на события и логи Kubernetes
	kubeIndicators := []string{
		"Event:",
		"Events:",
		"kubectl",
		"pod/",
		"deployment/",
		"service/",
		"configmap/",
		"secret/",
		"namespace/",
		"Warning",
		"Normal",
		"FailedMount",
		"Scheduled",
		"Pulled",
		"Created",
		"Started",
		"Killing",
		"logs",
		"NAMESPACE",
		"NAME",
		"READY",
		"STATUS",
		"RESTARTS",
		"AGE",
	}

	lineLower := strings.ToLower(line)
	for _, indicator := range kubeIndicators {
		if strings.Contains(lineLower, strings.ToLower(indicator)) {
			return "[kube]"
		}
	}

	// По умолчанию все остальное считаем выводом ArgoCD
	return "[argo]"
}

// RunArgoWait запускает `argocd app wait <app> --app-namespace=<ns>`,
// пробрасывает stdout/stderr и возвращает код выхода подпроцесса.
// В подпроцесс пробрасываются все переменные окружения с префиксом "ARGO".
func RunArgoWait(ctx context.Context, argocdBin, appName, namespace string) int {
	args := []string{"app", "wait", appName, "--app-namespace=" + namespace, "--health", "--sync", "--grpc-web"}
	fmt.Fprintln(os.Stderr, "[exec]", argocdBin, strings.Join(args, " "))

	cmd := exec.CommandContext(ctx, argocdBin, args...)

	// Получаем pipes для stdout и stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка создания stdout pipe: %v\n", err)
		return 1
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка создания stderr pipe: %v\n", err)
		return 1
	}

	// Пробрасываем окружение: только ARGO_*
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

	// Запускаем команду
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Ошибка запуска команды: %v\n", err)
		return 1
	}

	var wg sync.WaitGroup

	// Обрабатываем stdout
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

	// Обрабатываем stderr
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			prefix := getOutputPrefix(line)
			fmt.Fprintf(os.Stderr, "%s %s\n", prefix, line)
		}
	}()

	// Ждем завершения обработки всех потоков
	wg.Wait()

	err = cmd.Wait()
	if err == nil {
		return 0
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode()
	}
	fmt.Fprintln(os.Stderr, "Ошибка запуска argocd:", err)
	return 1
}
