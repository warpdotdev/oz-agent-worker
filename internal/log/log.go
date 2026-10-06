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

type sinkKey struct{}

func WithSink(ctx context.Context, sink func(context.Context, string, string) string) context.Context {
	return context.WithValue(ctx, sinkKey{}, sink)
}

func message(ctx context.Context, level, format string, args ...any) string {
	text := fmt.Sprintf(format, args...)
	if sink, ok := ctx.Value(sinkKey{}).(func(context.Context, string, string) string); ok {
		text = sink(ctx, level, text)
	}
	return text
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
	if zerolog.GlobalLevel() <= zerolog.DebugLevel {
		log.Debug().Msg(message(ctx, "debug", format, args...))
	}
}

func Infof(ctx context.Context, format string, args ...any) {
	log.Info().Msg(message(ctx, "info", format, args...))
}

func Warnf(ctx context.Context, format string, args ...any) {
	log.Warn().Msg(message(ctx, "warn", format, args...))
}

func Errorf(ctx context.Context, format string, args ...any) {
	log.Error().Msg(message(ctx, "error", format, args...))
}

func Fatalf(ctx context.Context, format string, args ...any) {
	log.Fatal().Msgf(format, args...)
	panic(fmt.Sprintf(format, args...))
}
