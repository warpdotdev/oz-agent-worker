package log

import (
	"context"
	"fmt"
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func init() {
	log.Logger = log.Output(zerolog.ConsoleWriter{
		Out:        os.Stderr,
		TimeFormat: "15:04:05.000",
	})
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
	log.Debug().Msgf(format, args...)
}

func Infof(ctx context.Context, format string, args ...any) {
	log.Info().Msgf(format, args...)
}

func Warnf(ctx context.Context, format string, args ...any) {
	log.Warn().Msgf(format, args...)
}

func Errorf(ctx context.Context, format string, args ...any) {
	log.Error().Msgf(format, args...)
}

func Fatalf(ctx context.Context, format string, args ...any) {
	log.Fatal().Msgf(format, args...)
	panic(fmt.Sprintf(format, args...))
}
