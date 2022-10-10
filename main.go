package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	arg "github.com/alexflint/go-arg"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// Options can be configured with environment variables and command line arguments.
type Options struct {
	RemoteWriteURL string `arg:"env:INGESTOR_REMOTE_WRITE_URL" default:"http://localhost:9201/api/v1/write"`
	MQTTBrokerURL  string `arg:"env:INGESTOR_MQTT_BROKER_URL" default:"tcp://localhost:1883"`
	MQTTUsername   string `arg:"env:INGESTOR_MQTT_USERNAME"`
	MQTTPassword   string `arg:"env:INGESTOR_MQTT_PASSWORD"`
	LogLevel       string `arg:"env:INGESTOR_LOG_LEVEL" default:"info" help:"trace, debug, info, warn, error, fatal, panic, disabled"`
}

func main() {
	// log.Info().Msgf("Starting Consumer %s (commit %s)", version, commit)

	ctx := context.Background()

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var opts Options

	arg.MustParse(&opts)

	// Setup logger.
	writer := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: "15:04:05",
	}

	logLevel, err := zerolog.ParseLevel(opts.LogLevel)
	if err != nil {
		log.Fatal().Msgf("Failed to parse log level: %s", err)
	}

	log.Logger = log.Output(writer).With().Timestamp().Logger().Level(logLevel)

	NewConsumer(opts).Run(ctx)

	log.Info().Msg("Consumer stopped")
}
