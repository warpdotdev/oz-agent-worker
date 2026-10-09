package log

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

var consoleOutput = zerolog.ConsoleWriter{
	Out:        os.Stderr,
	TimeFormat: "15:04:05.000",
}

func init() {
	log.Logger = log.Output(consoleOutput)
	zerolog.DefaultContextLogger = &log.Logger
}

func WithOutput(ctx context.Context, output io.Writer) context.Context {
	return log.Ctx(ctx).Output(zerolog.MultiLevelWriter(consoleOutput, output)).WithContext(ctx)
}

// SetLevel configures the global log level
func SetLevel(level string) {
	var logLevel zerolog.Level
	switch level {
	case "debug":
		logLevel = zerolog.DebugLevel
	case "info":
		logLevel = zerolog.InfoLevel
	case "warn":
		logLevel = zerolog.WarnLevel
	case "error":
		logLevel = zerolog.ErrorLevel
	default:
		logLevel = zerolog.InfoLevel
	}
	zerolog.SetGlobalLevel(logLevel)
}

func Debugf(ctx context.Context, format string, args ...any) {
	log.Ctx(ctx).Debug().Msgf(format, args...)
}

func Infof(ctx context.Context, format string, args ...any) {
	log.Ctx(ctx).Info().Msgf(format, args...)
}

func Warnf(ctx context.Context, format string, args ...any) {
	log.Ctx(ctx).Warn().Msgf(format, args...)
}

func Errorf(ctx context.Context, format string, args ...any) {
	log.Ctx(ctx).Error().Msgf(format, args...)
}

func Fatalf(ctx context.Context, format string, args ...any) {
	log.Ctx(ctx).Fatal().Msgf(format, args...)
	panic(fmt.Sprintf(format, args...))
}
