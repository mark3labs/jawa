// Command submit submits one task and logs its status and outcome.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	natschannel "github.com/mark3labs/bonnie/channel/nats"
	bonnienats "github.com/mark3labs/bonnie/client/nats"
	gonats "github.com/nats-io/nats.go"
)

func main() {
	if err := run(); err != nil {
		log.Print(err)
		os.Exit(1)
	}
}

func run() error {
	id := flag.String("id", "", "Task ID (reuse only for the same task)")
	text := flag.String("text", "", "Task instructions")
	root := flag.String("root", "bonnie", "NATS root subject")
	worker := flag.String("worker", "", "Target worker ID; empty uses the shared queue")
	timeout := flag.Duration("timeout", 30*time.Minute, "Maximum time to wait; does not cancel the worker")
	flag.Parse()
	if *text == "" {
		return errors.New("-text is required")
	}
	if *id == "" {
		*id = uuid.NewString()
	}
	url := os.Getenv("NATS_URL")
	if url == "" {
		url = "nats://127.0.0.1:4222"
	}
	opts := []gonats.Option{gonats.Name("jawa-submit"), gonats.Timeout(10 * time.Second)}
	if user := os.Getenv("NATS_USERNAME"); user != "" {
		opts = append(opts, gonats.UserInfo(user, os.Getenv("NATS_PASSWORD")))
	}
	nc, err := gonats.Connect(url, opts...)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer nc.Close()
	// Unique readers avoid competing with another application's consumers.
	// Remove these readers after use so CLI invocations do not accumulate them.
	reader := "jawa-cli-" + uuid.NewString()
	client, err := bonnienats.New(nc, bonnienats.Config{RootSubject: *root, ResultConsumer: reader, EventConsumer: reader, CreateStream: false, TargetedTasks: true})
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	defer func() {
		js, err := nc.JetStream()
		if err != nil {
			log.Printf("consumer cleanup: %v", err)
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for _, stream := range []string{natschannel.DefaultResultStreamName(*root + ".results"), natschannel.DefaultEventStreamName(*root + ".events")} {
			if err := js.DeleteConsumer(stream, reader, gonats.Context(cleanupCtx)); err != nil {
				log.Printf("cleanup consumer %s on %s: %v", reader, stream, err)
			}
		}
	}()
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(signalCtx, *timeout)
	defer cancel()
	errs := make(chan error, 2)
	outcomes := make(chan bonnienats.Outcome, 1)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		seen := map[string]bool{}
		errs <- client.ConsumeEvents(ctx, func(_ context.Context, e bonnienats.StatusEvent) error {
			if e.TaskID != *id || seen[e.EventID] {
				return nil
			}
			seen[e.EventID] = true
			if e.Type == "task_accepted" {
				log.Printf("picked up: task=%s worker=%s run=%s attempt=%s", e.TaskID, e.WorkerID, e.RunID, e.AttemptID)
			} else {
				log.Printf("status: task=%s run=%s state=%s seq=%d", e.TaskID, e.RunID, e.State, e.Seq)
			}
			return nil
		})
	}()
	go func() {
		defer wg.Done()
		errs <- client.Consume(ctx, func(_ context.Context, o bonnienats.Outcome) error {
			if o.TaskID != *id {
				return nil
			}
			select {
			case outcomes <- o:
			default:
			}
			return nil
		})
	}()
	defer func() { cancel(); wg.Wait() }()
	task := bonnienats.Task{TaskID: *id, Text: *text}
	if *worker == "" {
		_, err = client.Submit(ctx, task)
	} else {
		_, err = client.SubmitTo(ctx, *worker, task)
	}
	if err != nil {
		return fmt.Errorf("submit %s: %w", *id, err)
	}
	log.Printf("stored by broker: task=%s target_worker=%q (empty means shared queue)", *id, *worker)
	log.Printf("temporary consumers: %s (result and event streams)", reader)
	select {
	case o := <-outcomes:
		data, err := json.MarshalIndent(o, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(data))
		if o.Error != "" {
			return fmt.Errorf("task failed: %s", o.Error)
		}
		return nil
	case err := <-errs:
		if err == nil {
			return errors.New("consumer stopped before receiving an outcome")
		}
		return fmt.Errorf("consume: %w", err)
	case <-ctx.Done():
		return fmt.Errorf("waiting for %s: %w (task may still run)", *id, ctx.Err())
	}
}
