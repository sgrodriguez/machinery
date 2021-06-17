package servicebus

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	servicebus "github.com/Azure/azure-service-bus-go"
	"github.com/RichardKnop/machinery/v1/brokers/iface"
	"github.com/RichardKnop/machinery/v1/common"
	"github.com/RichardKnop/machinery/v1/config"
	"github.com/RichardKnop/machinery/v1/log"
	"github.com/RichardKnop/machinery/v1/tasks"
)

// Broker struct to hold all service bus related stuff
type Broker struct {
	common.Broker
	service      *servicebus.Namespace
	publishQueue *servicebus.Queue
	processingWG sync.WaitGroup // use wait group to make sure task processing completes on interrupt signal

	stopReceiving chan struct{}
}

// New creates a new broker
func New(cnf *config.Config) (iface.Broker, error) {
	b := &Broker{Broker: common.NewBroker(cnf), stopReceiving: make(chan struct{})}
	if cnf.ServiceBus != nil && cnf.ServiceBus.Client != nil {
		b.service = cnf.ServiceBus.Client
	} else {
		ns, err := servicebus.NewNamespace(servicebus.NamespaceWithConnectionString(cnf.Broker))
		if err != nil {
			return nil, err
		}
		b.service = ns
	}
	ctx := context.Background()
	_, err := b.service.NewQueueManager().Get(ctx, cnf.DefaultQueue)
	if err != nil {
		if _, ok := err.(servicebus.ErrNotFound); ok {
			return nil, fmt.Errorf("queue %s does not exist", cnf.DefaultQueue)
		}
		return nil, err
	}
	queue, err := b.service.NewQueue(b.GetConfig().DefaultQueue)
	if err != nil {
		return nil, err
	}
	b.publishQueue = queue
	return b, nil
}

// StartConsuming ...
func (b *Broker) StartConsuming(consumerTag string, concurrency int, taskProcessor iface.TaskProcessor) (bool, error) {
	b.Broker.StartConsuming(consumerTag, concurrency, taskProcessor)

	ctx, cancel := context.WithCancel(context.Background())

	queue := b.publishQueue
	var err error
	// we need a new queue connection with prefetch count
	if concurrency > 1 {
		queue, err = b.service.NewQueue(b.GetConfig().DefaultQueue, servicebus.QueueWithPrefetchCount(uint32(concurrency)))
		if err != nil {
			return false, err
		}
	}

	// Define msg chan
	msgChan := make(chan *servicebus.Message, concurrency)
	// Define a function that should be executed when a message is received.
	var concurrentHandler servicebus.HandlerFunc = func(ctx context.Context, msg *servicebus.Message) error {
		msgChan <- msg
		return nil
	}

	// Define msg workers
	for i := 0; i < concurrency; i++ {
		go func() {
			for msg := range msgChan {
				b.processingWG.Add(1)
				b.consumeOne(context.Background(), msg, taskProcessor)
				b.processingWG.Done()
			}
		}()
	}

	go func() {
		<-b.GetStopChan()
		cancel()
	}()

	for {
		err := queue.Receive(ctx, concurrentHandler)
		if err == nil {
			break
		}

		log.ERROR.Printf("Error when receiving messages. Error: %v", err)
		continue
	}

	close(b.stopReceiving)

	close(msgChan)

	return b.GetRetry(), nil
}

// StopConsuming ...
func (b *Broker) StopConsuming() {
	b.Broker.StopConsuming()

	<-b.stopReceiving

	// Wait for all processing tasks to finish
	b.processingWG.Wait()

}

// Publish message to queue
func (b *Broker) Publish(ctx context.Context, sig *tasks.Signature) error {
	// Adjust routing key (this decides which queue the message will be published to)
	b.AdjustRoutingKey(sig)
	sigMarshaled, err := json.Marshal(sig)
	if err != nil {
		return fmt.Errorf("JSON marshal error: %s", err)
	}

	msg := servicebus.NewMessage(sigMarshaled)
	// Set message id to machinery task UUID
	msg.ID = sig.UUID
	// Check the ETA signature field, if it is set and it is in the future,
	// delay the task
	if sig.ETA != nil {
		now := time.Now().UTC()
		if sig.ETA.After(now) {
			msg.ScheduleAt(*sig.ETA)
		}
	}

	err = b.publishQueue.Send(ctx, msg)
	if err != nil {
		log.ERROR.Printf("Error when sending a message: %v", err)
		return err
	}
	return nil
}

func (b *Broker) consumeOne(ctx context.Context, msg *servicebus.Message, taskProcessor iface.TaskProcessor) error {
	if len(msg.Data) == 0 {
		log.ERROR.Printf("received an empty message, the msg was %v", msg)
		return msg.DeadLetter(ctx, fmt.Errorf("empty message data"))
	}
	sig := new(tasks.Signature)
	decoder := json.NewDecoder(bytes.NewBuffer(msg.Data))
	decoder.UseNumber()
	if err := decoder.Decode(sig); err != nil {
		log.ERROR.Printf("unmarshal error. the message is %v", msg)
		return msg.DeadLetter(ctx, fmt.Errorf("unmarshal msg data error"))
	}
	// If the task is not registered return an error
	// and leave the message in the queue
	if !b.IsTaskRegistered(sig.Name) {
		log.ERROR.Printf("task %s is not registered", sig.Name)
		if sig.IgnoreWhenTaskNotRegistered {
			return msg.DeadLetter(ctx, fmt.Errorf("task %s is not registered", sig.Name))
		}
		return msg.Abandon(ctx)
	}

	err := taskProcessor.Process(sig)
	if err != nil {
		log.ERROR.Printf("failed process of task %v", err)
		return msg.Abandon(ctx)
	}
	// Call Complete() after successfully consuming and processing the message
	return msg.Complete(ctx)
}