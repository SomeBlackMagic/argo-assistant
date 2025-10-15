# Argo Assistant

## Description

Argo Assistant is a utility for monitoring and aggregating logs from Kubernetes workloads, integrated with ArgoCD. The application tracks Pods in a Kubernetes cluster by the `argocd.argoproj.io/tracking-id` annotation and streams their logs in real-time, in parallel with executing the `argocd app wait` command.

## Key Features

### 1. Workload Tracking
The application automatically discovers and tracks the following types of Kubernetes resources:
- **Deployments**
- **StatefulSets**
- **DaemonSets** (partially implemented)
- **Jobs** (partially implemented)
- **CronJobs** (partially implemented)

### 2. Log Streaming
- Tracks logs from all containers (init and regular) in Pods
- Distinguishes between new and existing Pods:
  - For new Pods: outputs all logs from startup
  - For existing Pods: outputs the last 10 lines
- Automatically tracks container restarts
- Prevents log duplication on restarts

### 3. Event Tracking
- Monitors Kubernetes events for tracked Pods
- Automatically maps event types to corresponding log levels
- Prevents event duplication

### 4. ArgoCD Integration
- Runs the `argocd app wait` command in parallel
- Prefixes ArgoCD output with the `[argo]` tag
- Passes only environment variables with the `ARGO*` prefix

## Architecture

### Core Components

#### Main (`main.go`)
Application entry point. Coordinates:
- Log streaming launch in a separate goroutine
- ArgoCD command execution
- Shutdown signal handling (SIGINT, SIGTERM)

#### KubeLogStreamer (`kubectl.go`)
Main orchestrator that:
- Creates and coordinates all components
- Sets up informers for Pods and Events
- Scans existing workloads
- Manages the monitoring lifecycle

#### Client (`client/kubernetes.go`)
Creates and configures the Kubernetes client:
- Attempts to use in-cluster configuration
- Falls back to KUBECONFIG

#### Informer Manager (`informer/manager.go`)
Manages Kubernetes informers:
- Creates and configures informers for Pods and Events
- Manages caching and synchronization

#### Pod Handler (`pod/handler.go`)
Handles Pod events:
- Checks if a Pod belongs to tracked workloads
- Processes init and regular containers
- Initiates log streaming

#### Scanner (`scanner/workload.go`)
Scans workloads:
- Finds workloads by tracking-id annotation
- Sets up watchers for different workload types
- Scans existing Pods

#### Streaming Coordinator (`streaming/coordinator.go`)
Coordinates logs:
- Manages active log streams
- Prevents stream duplication
- Tracks container restarts

#### Log Streamer (`logsPipe/streamer.go`)
Performs actual log streaming:
- Waits for container readiness
- Streams logs via Kubernetes API
- Formats log output

#### Event Handler (`eventsPipe/handler.go`)
Handles Kubernetes events:
- Filters events only for tracked Pods
- Prevents event duplication
- Maps event types to log levels

#### Workload Finder (`workloadFinder/finder.go`)
Determines Pod ownership by workloads:
- Tracks UIDs of top-level workloads
- Traverses the ownerReferences chain
- Supports various controller types (ReplicaSet, Deployment, StatefulSet, etc.)

#### Workloads Package (`workloads/`)
Provides workload-specific logic for each type:
- Matchers for tracking-id verification
- Watchers for informers
- Scanners for existing resources

## Usage

```bash
./argo-assistant sync <app-name> <app-namespace>
```

### Parameters
- `<app-name>` - ArgoCD application name (used as tracking-id)
- `<app-namespace>` - Application namespace in Kubernetes

### Environment Variables

#### General
- `ARGOCD` - Path to argocd binary (default: `argocd`)

#### Logging
- `LOG_FORMAT` - Log format: `text` or `json` (default: `text`)
- `LOG_LEVEL` - Log level: `debug`, `info`, `warn`, `error` (default: `info`)
- `LOG_FULL_TIMESTAMP` - Display full timestamps (default: `true`)
- `LOG_TIMESTAMP_FORMAT` - Timestamp format (default: `2006-01-02 15:04:05`)
- `LOG_DISABLE_COLORS` - Disable colors in logs (default: `false`)
- `LOG_FORCE_COLORS` - Force enable colors (default: `false`)
- `LOG_DISABLE_LEVEL_TRUNCATION` - Don't truncate log level names (default: `false`)
- `LOG_PAD_LEVEL_TEXT` - Pad log level names with spaces (default: `false`)

#### ArgoCD (passed to subprocess)
All variables with the `ARGO*` prefix, for example:
- `ARGOCD_SERVER`
- `ARGOCD_AUTH_TOKEN`
- `ARGOCD_OPTS`

## How It Works

1. **Initialization**: Creates Kubernetes client and configures all components
2. **Informer Setup**: Creates watchers for Pods, Events and workloads
3. **Scanning**: Scans existing workloads with the required tracking-id annotation
4. **Monitoring**:
   - Tracks new and updated workloads
   - Finds associated Pods for discovered workloads
   - Starts log streaming for each container in the Pod
   - Tracks Kubernetes events
5. **Parallel Execution**: Simultaneously executes the `argocd app wait` command
6. **Termination**: When the ArgoCD command completes or a signal is received, all streams are stopped

## Implementation Details

### Tracking ID
The application uses the `argocd.argoproj.io/tracking-id` annotation to determine resource ownership by a specific ArgoCD application.

### Ownership Chain
To determine if a Pod belongs to a top-level workload, the application traverses the ownerReferences chain:
```
Pod → ReplicaSet → Deployment (top-level)
Pod → StatefulSet (top-level)
Pod → Job → CronJob (top-level)
```

### Deduplication
- Each log stream is identified by a key: `{PodName, Namespace, ContainerName, RestartCount}`
- Events are tracked by timestamp to prevent duplicate output

### Graceful Shutdown
The application properly handles termination signals:
- Stops all active log streams
- Terminates the ArgoCD subprocess
- Close informers

## Dependencies

- `k8s.io/client-go` - Kubernetes client
- `k8s.io/api` - Kubernetes API types
- `github.com/sirupsen/logrus` - Structured logging
