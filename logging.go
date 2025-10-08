// logging.go
package main

import (
	"os"
	"strings"

	"github.com/sirupsen/logrus"
)

var logger = logrus.New()

func init() {
	// Configure log format based on environment variables
	logFormat := strings.ToLower(os.Getenv("LOG_FORMAT"))
	if logFormat == "json" {
		logger.SetFormatter(&logrus.JSONFormatter{
			TimestampFormat: getTimestampFormat(),
		})
	} else {
		// Default to text format
		logger.SetFormatter(&logrus.TextFormatter{
			FullTimestamp:          getFullTimestamp(),
			TimestampFormat:        getTimestampFormat(),
			DisableColors:          getDisableColors(),
			ForceColors:            getForceColors(),
			DisableLevelTruncation: getDisableLevelTruncation(),
			PadLevelText:           getPadLevelText(),
		})
	}

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

// getFullTimestamp returns the value of LOG_FULL_TIMESTAMP environment variable (default: true)
func getFullTimestamp() bool {
	val := os.Getenv("LOG_FULL_TIMESTAMP")
	if val == "" {
		return true
	}
	return strings.ToLower(val) == "true"
}

// getTimestampFormat returns the value of LOG_TIMESTAMP_FORMAT environment variable (default: "2006-01-02 15:04:05")
func getTimestampFormat() string {
	val := os.Getenv("LOG_TIMESTAMP_FORMAT")
	if val == "" {
		return "2006-01-02 15:04:05"
	}
	return val
}

// getDisableColors returns the value of LOG_DISABLE_COLORS environment variable (default: false)
func getDisableColors() bool {
	val := os.Getenv("LOG_DISABLE_COLORS")
	return strings.ToLower(val) == "true"
}

// getForceColors returns the value of LOG_FORCE_COLORS environment variable (default: false)
func getForceColors() bool {
	val := os.Getenv("LOG_FORCE_COLORS")
	return strings.ToLower(val) == "true"
}

// getDisableLevelTruncation returns the value of LOG_DISABLE_LEVEL_TRUNCATION environment variable (default: false)
func getDisableLevelTruncation() bool {
	val := os.Getenv("LOG_DISABLE_LEVEL_TRUNCATION")
	return strings.ToLower(val) == "true"
}

// getPadLevelText returns the value of LOG_PAD_LEVEL_TEXT environment variable (default: false)
func getPadLevelText() bool {
	val := os.Getenv("LOG_PAD_LEVEL_TEXT")
	return strings.ToLower(val) == "true"
}
