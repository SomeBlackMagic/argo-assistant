// workloads/logger.go
package workloads

import (
	"github.com/sirupsen/logrus"
)

// Use the same logger instance from the main package
// This will be set by the main package during initialization
var logger *logrus.Logger

func SetLogger(l *logrus.Logger) {
	logger = l
}
