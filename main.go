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

// Variables set during build.
//
//nolint:gochecknoglobals
var (
	version = "0.1"
	commit  = "unset"
)

// Options can be configured with environment variables and command line arguments.
//
//nolint:lll
type Options struct {
	RemoteWriteURL  string `arg:"--remote-write-url,env:INGESTOR_REMOTE_WRITE_URL" default:"http://localhost:9201/api/v1/write"`
	MQTTBrokerURL   string `arg:"--mqtt-broker-url,env:INGESTOR_MQTT_BROKER_URL" default:"tcp://localhost:1883"`
	MQTTUsername    string `arg:"--mqtt-username,env:INGESTOR_MQTT_USERNAME" default:"default"`
	MQTTPassword    string `arg:"--mqtt-password,env:INGESTOR_MQTT_PASSWORD"`
	MQTTSSLInsecure bool   `arg:"--mqtt-ssl-insecure,env:INGESTOR_MQTT_SSL_INSECURE"`
	MQTTCAFile      string `arg:"--mqtt-ca-file,env:INGESTOR_MQTT_CA_FILE"`
	LogLevel        string `arg:"--log-level,env:INGESTOR_LOG_LEVEL" default:"info"`
}

// Version implements --version argument.
func (Options) Version() string {
	return version
}

func main() {
	// Parse arguments.
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

	// Run the ingestor.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	log.Info().Msgf("Starting SquirrelDB Ingestor version %s (commit %s)", version, commit)

	NewIngestor(opts).Run(ctx)

	log.Info().Msg("SquirrelDB Ingestor stopped")
}
