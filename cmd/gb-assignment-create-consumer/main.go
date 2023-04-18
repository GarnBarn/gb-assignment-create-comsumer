package main

import (
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/GarnBarn/common-go/database"
	"github.com/GarnBarn/common-go/httpserver"
	"github.com/GarnBarn/gb-assignment-consumer/cmd/gb-assignment-create-consumer/processor"
	"github.com/GarnBarn/gb-assignment-consumer/pkg/config"
	"github.com/GarnBarn/gb-assignment-consumer/pkg/repository"
	"github.com/sirupsen/logrus"
	"github.com/wagslane/go-rabbitmq"
)

var (
	appConfig config.Config
)

func init() {
	appConfig = config.Load()
}

func main() {
	// Connect RabbitMQ
	conn, err := rabbitmq.NewConn(
		appConfig.RABBITMQ_CONNECTION,
		rabbitmq.WithConnectionOptionsLogging,
	)
	if err != nil {
		logrus.Fatal(err)
	}

	// Connect Database
	db, err := database.Conn(appConfig.MYSQL_CONNECTION_STRING)
	if err != nil {
		logrus.Fatalln("Can't connect to database: ", err)
		return
	}

	// Start HealthChecking Server
	go func() {
		httpServer := httpserver.NewHttpServer()
		logrus.Info("Listening and serving HTTP on :", appConfig.HTTP_SERVER_PORT)
		httpServer.Run(fmt.Sprint(":", appConfig.HTTP_SERVER_PORT))
	}()

	publisher, err := rabbitmq.NewPublisher(
		conn,
		rabbitmq.WithPublisherOptionsLogging,
		rabbitmq.WithPublisherOptionsExchangeName(appConfig.RABBITMQ_ASSIGNMENT_EXCHANGE),
	)
	if err != nil {
		logrus.Fatal(err)
	}

	// Create Repository
	assignmentRepository := repository.NewAssignmentRepository(db)

	// Create Processor
	processor := processor.NewProcessor(publisher, assignmentRepository)

	consumer, err := rabbitmq.NewConsumer(
		conn,
		func(d rabbitmq.Delivery) (action rabbitmq.Action) {
			logrus.Info("Start Processing the message")
			defer logrus.Info("End Processing the message")
			err := processor.Process(d)

			if err != nil {
				value, ok := d.Headers["x-retry"]
				retryCount := 0
				if ok {
					convertResult, err := strconv.Atoi(fmt.Sprint(value))
					if err != nil {
						convertResult = 0
					}
					retryCount = convertResult
				}

				retryCount++

				rabbitMqHeaderTable := rabbitmq.Table{
					"x-retry": retryCount,
				}

				if retryCount >= appConfig.RABBITMQ_MAXIMUM_RETRY {
					logrus.Warn("Maximum retry exceeded, Publishing the message to dead lettering exchange")
					publisher.Publish(d.Body, []string{d.RoutingKey},
						rabbitmq.WithPublishOptionsExchange(appConfig.RABBITMQ_DEAD_LETTERING_EXCHANGE),
						rabbitmq.WithPublishOptionsContentType(d.ContentType),
						rabbitmq.WithPublishOptionsHeaders(rabbitMqHeaderTable),
					)
					return rabbitmq.Ack
				}

				logrus.Warn("Publishing message back to exchange, ")
				publisher.Publish(d.Body, []string{d.RoutingKey},
					rabbitmq.WithPublishOptionsExchange(appConfig.RABBITMQ_ASSIGNMENT_EXCHANGE),
					rabbitmq.WithPublishOptionsContentType(d.ContentType),
					rabbitmq.WithPublishOptionsHeaders(rabbitMqHeaderTable),
				)
			}

			return rabbitmq.Ack
		},
		appConfig.RABBITMQ_ASSIGNMENT_CREATE_QUEUE,
		rabbitmq.WithConsumerOptionsQueueDurable,
	)
	if err != nil {
		logrus.Fatal(err)
	}

	gracefulStop := make(chan os.Signal, 1)
	signal.Notify(gracefulStop, syscall.SIGTERM)
	signal.Notify(gracefulStop, syscall.SIGINT)
	<-gracefulStop

	logrus.Info("Gracefully shutting down.")
	consumer.Close()
	publisher.Close()
	conn.Close()

	logrus.Info("Successfully shutting down the amqp. Bye!!")
}
