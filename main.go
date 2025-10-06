package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

func main() {
	argocd := getEnvStrict("ARGOCD", "argocd")

	if len(os.Args) < 3 {
		fmt.Fprintf(os.Stderr, "Usage: %s <app-name> <app-namespace>\n", os.Args[0])
		os.Exit(1)
	}
	appName := os.Args[1]
	namespace := os.Args[2]

	// Контекст с отменой и обработка сигналов
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		cancel()
	}()

	// Параллельно — стрим логов, передаем app-name для отслеживания по tracking-id аннотации
	var wg sync.WaitGroup
	logCtx, logCancel := context.WithCancel(ctx)
	wg.Add(1)
	go func() {
		defer wg.Done()
		streamer := NewKubeLogStreamer(namespace, appName, false, os.Stdout)
		if err := streamer.StreamLogsByTrackingID(logCtx); err != nil && err != context.Canceled {
			fmt.Fprintf(os.Stderr, "Ошибка стрима логов: %v\n", err)
		}
	}()

	// Основной подпроцесс: argocd app wait ...
	exitCode := RunArgoWait(ctx, argocd, appName, namespace)

	// Завершение лог-стримера
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
		fmt.Fprintf(os.Stderr, "Не задана обязательная переменная окружения %s\n", name)
		os.Exit(1)
	}
	return v
}
