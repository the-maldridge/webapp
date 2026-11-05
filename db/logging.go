package db

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// slogLogger adapts GORM's logger interface to log/slog so that query
// logging shares the application's handler, level, and formatting
// instead of writing an unstructured second stream to stdout.
//
// The default GORM logger writes through the stdlib log package with
// its own formatting and its own level. Applications that configure
// structured logging then get two incompatible logs to correlate.
type slogLogger struct {
	threshold time.Duration
	level     slog.Level
}

// newSlogLogger returns a logger that reports queries slower than
// threshold. Every query is emitted at Debug, slow queries at Warn,
// and failing queries at Error. A threshold of zero reports duration
// but never classifies a query as slow.
func newSlogLogger(threshold time.Duration) gormlogger.Interface {
	return &slogLogger{threshold: threshold, level: slog.LevelDebug}
}

// LogMode satisfies gorm logger.Interface. The level GORM asks for is
// mapped onto the slog level below which query logs are suppressed; the
// slow-query threshold is preserved.
func (l *slogLogger) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	out := *l
	switch level {
	case gormlogger.Silent:
		out.level = slog.Level(discardLevel)
	case gormlogger.Error:
		out.level = slog.LevelError
	case gormlogger.Warn:
		out.level = slog.LevelWarn
	case gormlogger.Info:
		out.level = slog.LevelInfo
	}
	return &out
}

// discardLevel is above slog.LevelError and exists so that GORM's
// Silent mode suppresses everything without a separate code path.
const discardLevel = slog.Level(1 << 30)

// Info satisfies gorm logger.Interface.
func (l *slogLogger) Info(ctx context.Context, msg string, args ...any) {
	if !l.enabled(slog.LevelInfo) {
		return
	}
	slog.InfoContext(ctx, "db: "+msg, args...)
}

// Warn satisfies gorm logger.Interface.
func (l *slogLogger) Warn(ctx context.Context, msg string, args ...any) {
	if !l.enabled(slog.LevelWarn) {
		return
	}
	slog.WarnContext(ctx, "db: "+msg, args...)
}

// Error satisfies gorm logger.Interface.
func (l *slogLogger) Error(ctx context.Context, msg string, args ...any) {
	if !l.enabled(slog.LevelError) {
		return
	}
	slog.ErrorContext(ctx, "db: "+msg, args...)
}

// Trace satisfies gorm logger.Interface. It is called once per
// statement with the elapsed time and the statement's error.
func (l *slogLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	elapsed := time.Since(begin)

	switch {
	case err != nil && !errors.Is(err, gorm.ErrRecordNotFound) && l.enabled(slog.LevelError):
		sql, rows := fc()
		slog.ErrorContext(ctx, "db: query failed",
			"elapsed", elapsed, "rows", rows, "error", err, "sql", sql)
	case l.threshold > 0 && elapsed > l.threshold && l.enabled(slog.LevelWarn):
		sql, rows := fc()
		slog.WarnContext(ctx, "db: slow query",
			"elapsed", elapsed, "threshold", l.threshold, "rows", rows, "sql", sql)
	case l.enabled(l.level):
		sql, rows := fc()
		slog.DebugContext(ctx, "db: query",
			"elapsed", elapsed, "rows", rows, "sql", sql)
	}
}

// enabled reports whether lvl would be emitted by the configured
// handler. Checking first avoids building argument slices, including
// the SQL string, for log lines that will be dropped.
func (l *slogLogger) enabled(lvl slog.Level) bool {
	return lvl >= l.level && slog.Default().Enabled(context.Background(), lvl)
}
