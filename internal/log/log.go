package log

import (
	"context"
	"fmt"
	"os"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

func init() {
	// Ensure zerolog writes to stderr in a human-readable format
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
}

func Debugf(ctx context.Context, format string, args ...interface{}) {
	log.Debug().Msgf(format, args...)
}

func Infof(ctx context.Context, format string, args ...interface{}) {
	log.Info().Msgf(format, args...)
}

func Warnf(ctx context.Context, format string, args ...interface{}) {
	log.Warn().Msgf(format, args...)
}

func Errorf(ctx context.Context, format string, args ...interface{}) {
	log.Error().Msgf(format, args...)
}

func Fatalf(ctx context.Context, format string, args ...interface{}) {
	log.Fatal().Msgf(format, args...)
	panic(fmt.Sprintf(format, args...))
}
