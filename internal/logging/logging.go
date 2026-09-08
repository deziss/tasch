// Package logging configures the daemon's structured logger.
//
// Operational events were previously a mix of log.Printf and bare fmt.Printf to stdout: no
// levels, no timestamps on half of them, and no job_id field, which made the output
// unparseable by Loki, ELK, or Splunk and impossible to correlate across the master and the
// worker that ran a job.
package logging

import (
	"log"
	"log/slog"
	"os"
	"strings"
)

// Setup installs the process-wide logger.
//
// format is "text" (default, human-readable) or "json". level is debug, info, warn, or error.
// It also redirects the standard log package, so output from dependencies such as memberlist
// lands in the same stream rather than bypassing the format entirely.
func Setup(format, level string) *slog.Logger {
	opts := &slog.HandlerOptions{Level: parseLevel(level)}

	var handler slog.Handler
	if strings.EqualFold(format, "json") {
		handler = slog.NewJSONHandler(os.Stderr, opts)
	} else {
		handler = slog.NewTextHandler(os.Stderr, opts)
	}

	logger := slog.New(handler)
	slog.SetDefault(logger)

	// Route the standard logger through slog too. memberlist and grpc log this way, and their
	// output previously escaped the configured format entirely.
	log.SetFlags(0)
	log.SetOutput(stdlogWriter{logger})

	return logger
}

// stdlogWriter adapts the standard log package onto slog.
type stdlogWriter struct{ logger *slog.Logger }

func (w stdlogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	// memberlist prefixes its own level, e.g. "[DEBUG] memberlist: ...". Map it so a debug-level
	// gossip trace does not surface as an info-level application event.
	switch {
	case strings.HasPrefix(msg, "[DEBUG]"):
		w.logger.Debug(strings.TrimSpace(strings.TrimPrefix(msg, "[DEBUG]")))
	case strings.HasPrefix(msg, "[INFO]"):
		w.logger.Info(strings.TrimSpace(strings.TrimPrefix(msg, "[INFO]")))
	case strings.HasPrefix(msg, "[WARN]"):
		w.logger.Warn(strings.TrimSpace(strings.TrimPrefix(msg, "[WARN]")))
	case strings.HasPrefix(msg, "[ERR]"), strings.HasPrefix(msg, "[ERROR]"):
		w.logger.Error(strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(msg, "[ERROR]"), "[ERR]")))
	default:
		w.logger.Info(msg)
	}
	return len(p), nil
}

func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// Job returns a logger tagged with a job id, so every line about one job can be correlated
// across the master and the worker that ran it.
func Job(jobID string) *slog.Logger {
	return slog.Default().With("job_id", jobID)
}
