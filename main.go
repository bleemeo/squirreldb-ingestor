package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/alexflint/go-arg"
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
	RemoteWriteURL         string   `arg:"--remote-write-url,env:INGESTOR_REMOTE_WRITE_URL"                   default:"http://localhost:9201/api/v1/write"`
	RemoteWriteUsername    string   `arg:"--remote-write-username,env:INGESTOR_REMOTE_WRITE_USERNAME"`
	RemoteWritePassword    string   `arg:"--remote-write-password,env:INGESTOR_REMOTE_WRITE_PASSWORD"`
	RemoteWriteBearerToken string   `arg:"--remote-write-bearer-token,env:INGESTOR_REMOTE_WRITE_BEARER_TOKEN"`
	RemoteWriteHeader      []string `arg:"--remote-write-header,separate,env:INGESTOR_REMOTE_WRITE_HEADER"`
	MQTTBrokerURL          []string `arg:"--mqtt-broker-url,env:INGESTOR_MQTT_BROKER_URL"`
	MQTTUsername           string   `arg:"--mqtt-username,env:INGESTOR_MQTT_USERNAME"                         default:"default"`
	MQTTPassword           string   `arg:"--mqtt-password,env:INGESTOR_MQTT_PASSWORD"`
	MQTTClientID           string   `arg:"--mqtt-client-id,env:INGESTOR_MQTT_CLIENT_ID"`
	MQTTSSLInsecure        bool     `arg:"--mqtt-ssl-insecure,env:INGESTOR_MQTT_SSL_INSECURE"`
	MQTTCAFile             string   `arg:"--mqtt-ca-file,env:INGESTOR_MQTT_CA_FILE"`
	MQTTID                 string   `arg:"--mqtt-id,env:INGESTOR_MQTT_ID"`
	HTTPAddress            string   `arg:"--http-address,env:INGESTOR_HTTP_ADDRESS"                           default:"0.0.0.0:8080"`
	LogLevel               string   `arg:"--log-level,env:INGESTOR_LOG_LEVEL"                                 default:"info"`
}

// Version implements --version argument.
func (Options) Version() string {
	return version
}

func main() {
	// Parse arguments.
	var opts Options

	arg.MustParse(&opts)

	// go-arg doesn't support default values for slices.
	if opts.MQTTBrokerURL == nil {
		opts.MQTTBrokerURL = []string{"tcp://localhost:1883"}
	}

	// The client ID defaults to the username to keep backward compatibility.
	if opts.MQTTClientID == "" {
		opts.MQTTClientID = opts.MQTTUsername
	}

	// Setup logger.
	writer := zerolog.ConsoleWriter{
		Out:        os.Stdout,
		TimeFormat: "2006-01-02 15:04:05.000",
	}

	logLevel, err := zerolog.ParseLevel(opts.LogLevel)
	if err != nil {
		log.Fatal().Msgf("Failed to parse log level: %s", err)
	}

	log.Logger = log.Output(writer).With().Timestamp().Logger().Level(logLevel)

	// Change the default time format of zerolog to allow millisecond precision.
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnixMs

	log.Info().Msgf("Starting SquirrelDB Ingestor version %s (commit %s)", version, commit)

	ingestor, err := NewIngestor(opts)
	if err != nil {
		log.Fatal().Err(err).Msg("Failed to initialize the ingestor")
	}

	// Run the ingestor.
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	ingestor.Run(ctx)

	log.Info().Msg("SquirrelDB Ingestor stopped")
}
