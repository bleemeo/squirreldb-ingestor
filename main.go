package main

import (
	"context"
	"log"
	"os/signal"
	"syscall"

	arg "github.com/alexflint/go-arg"
)

// Options can be configured with environment variables and command line arguments.
type Options struct {
	RemoteWriteURL string `arg:"env:INGESTOR_REMOTE_WRITE_URL" default:"http://localhost:9201/api/v1/write"`
	MQTTBrokerURL  string `arg:"env:INGESTOR_MQTT_BROKER_URL" default:"tcp://localhost:1883"`
	MQTTUsername   string `arg:"env:INGESTOR_MQTT_USERNAME"`
	MQTTPassword   string `arg:"env:INGESTOR_MQTT_PASSWORD"`
}

func main() {
	log.Println("Starting Consumer")

	ctx := context.Background()

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var opts Options

	arg.MustParse(&opts)

	NewConsumer(opts).Run(ctx)

	log.Println("Consumer stopped")
}
