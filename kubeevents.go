package main

import (
	"regexp"

	v1 "k8s.io/api/core/v1"
)

type LogLevel int

const (
	Debug LogLevel = iota
	Info
	Warn
	Error
)

func (l LogLevel) String() string {
	switch l {
	case Debug:
		return "debug"
	case Info:
		return "info"
	case Warn:
		return "warn"
	case Error:
		return "error"
	default:
		return "info"
	}
}

// Точные соответствия (override).
// Можно расширять по мере необходимости.
var reasonExactToLevel = map[string]LogLevel{
	// INFO
	"Scheduled":               Info,
	"Pulling":                 Info,
	"Pulled":                  Info,
	"Created":                 Info,
	"Started":                 Info,
	"SuccessfulCreate":        Info,
	"SuccessfulDelete":        Info,
	"SuccessfulUpdate":        Info,
	"ScalingReplicaSet":       Info,
	"SuccessfulAttachVolume":  Info,
	"NodeReady":               Info,
	"Sync":                    Info,
	"Completed":               Info,
	"Starting":                Info,
	"NodeHasSufficientMemory": Info,
	"NodeHasNoDiskPressure":   Info,

	// WARNING (recoverable)
	"BackOff":                   Warn,
	"CrashLoopBackOff":          Warn,
	"FailedScheduling":          Warn,
	"FailedMount":               Warn,
	"FailedAttachVolume":        Warn,
	"Unhealthy":                 Warn,
	"NodeNotReady":              Warn,
	"NodeHasInsufficientMemory": Warn,
	"NodeHasDiskPressure":       Warn,
	"NodeHasNetworkUnavailable": Warn,
	"Preempted":                 Warn,
	"Evicted":                   Warn,
	"NodeSelectorMismatch":      Warn,
	"ReconcileError":            Warn,
	"CNIConfigError":            Warn,
	"CNINotInitialized":         Warn,

	// ERROR (требует вмешательства)
	"FailedCreate":          Error,
	"FailedDelete":          Error,
	"ContainerCannotRun":    Error,
	"OOMKilled":             Error,
	"DeadlineExceeded":      Error,
	"BackoffLimitExceeded":  Error,
	"NetworkUnavailable":    Error,
	"ErrorFetchingResource": Error,
	"Failed":                Error,
	"FailedDaemonPod":       Error,
}

// Регексп-правила для общих паттернов reason.
// Порядок важен: первое совпадение выигрывает.
var reasonRegexToLevel = []struct {
	re    *regexp.Regexp
	level LogLevel
}{
	// Error-паттерны
	{regexp.MustCompile(`(?i)cannotrun`), Error},
	{regexp.MustCompile(`(?i)oomkilled`), Error},
	{regexp.MustCompile(`(?i)deadlineexceeded`), Error},
	{regexp.MustCompile(`(?i)backofflimitexceeded`), Error},
	{regexp.MustCompile(`(?i)^failed($|[A-Z_])`), Error}, // Failed*, FailedXxx
	{regexp.MustCompile(`(?i)networkunavailable`), Error},
	{regexp.MustCompile(`(?i)error`), Error},

	// Warning-паттерны
	{regexp.MustCompile(`(?i)crashloopbackoff`), Warn},
	{regexp.MustCompile(`(?i)\bbackoff\b`), Warn},
	{regexp.MustCompile(`(?i)failed(scheduling|mount|attachvolume)`), Warn},
	{regexp.MustCompile(`(?i)unhealthy`), Warn},
	{regexp.MustCompile(`(?i)evicted`), Warn},
	{regexp.MustCompile(`(?i)notready`), Warn},
	{regexp.MustCompile(`(?i)diskpressure|insufficientmemory|networkunavailable`), Warn},
	{regexp.MustCompile(`(?i)cninotinitialized|cniconfigerror`), Warn},

	// Info-паттерны (на всякий случай)
	{regexp.MustCompile(`(?i)successful(create|delete|update|attach|mount)?`), Info},
	{regexp.MustCompile(`(?i)scheduled|pulling|pulled|created|started|completed|sync`), Info},
}

// MapEventToLogLevel — основной вход.
func MapEventToLogLevel(ev *v1.Event) LogLevel {
	if ev == nil {
		return Info
	}
	reason := ev.Reason

	// 1) Точное соответствие
	if lvl, ok := reasonExactToLevel[reason]; ok {
		return lvl
	}

	// 2) Регексп-паттерны
	for _, r := range reasonRegexToLevel {
		if r.re.MatchString(reason) {
			return r.level
		}
	}

	// 3) Fallback по типу события
	switch ev.Type { // v1.Event.Type обычно "Normal" или "Warning"
	case v1.EventTypeWarning:
		return Warn
	case v1.EventTypeNormal:
		return Info
	default:
		return Info
	}
}
