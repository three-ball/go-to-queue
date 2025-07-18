package gotoqueue

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"
)

type ContextKey string

// MetadataKey wraps a string to create a unique context key
func MetadataKey(key string) ContextKey {
	return ContextKey("metadata:" + key)
}

// GetMetadata retrieves metadata from context using the custom key type
func GetMetadata(ctx context.Context, key string) (interface{}, bool) {
	value := ctx.Value(MetadataKey(key))
	return value, value != nil
}

// Worker is responsible for processing items from the queue.
// Each worker has a unique ID, a channel to receive items, and a stop signal.
// Workers will process items in the order they are received, and they will only process one item at a time.
// The stop signal is used to gracefully shut down the worker.
// The WaitGroup is used to wait for the worker to finish processing before shutting down.
type Worker struct {
	id           int
	queue        chan QueueItem
	stopSignal   chan struct{}
	wg           *sync.WaitGroup
	panicHandler PanicHandler
	logger       Logger // Custom logger interface
}

// start is the main worker loop - processes items in FIFO order
// and executes the function associated with each item.
// It listens for items on the queue and processes them until it receives a stop signal.
// If a stop signal is received, it drains the queue and processes any remaining items before shutting down.
func (w *Worker) start() {
	defer w.wg.Done()

	for {
		select {
		case item := <-w.queue:
			// Check if item is expired before processing
			if item.IsExpired() {
				w.logger.Debugf("Worker %d: Skipping expired item with key: %s (age: %v)",
					w.id, item.key, item.GetAge())
				continue
			}

			// Check if item context is cancelled before processing
			if item.IsCancelled() {
				w.logger.Debugf("Worker %d: Skipping cancelled item with key: %s", w.id, item.key)
				continue
			}

			// Execute the function with context awareness
			if item.fn != nil {
				// If item has context, monitor for cancellation during execution
				if item.ctx != nil {
					done := make(chan struct{})
					var recovered bool
					var panicValue interface{}

					go func() {
						defer close(done)
						recovered, panicValue = w.safeExecute(&item)
					}()

					select {
					case <-done:
						if recovered {
							w.logger.Errorf("Worker %d: Panic recovered for key: %s - %v",
								w.id, item.key, panicValue)
						} else {
							w.logger.Debugf("Worker %d: Completed item with key: %s (age: %v)",
								w.id, item.key, item.GetAge())
						}
					case <-item.ctx.Done():
						// Context was cancelled during execution
						w.logger.Infof("Worker %d: Item cancelled during execution with key: %s (reason: %v)",
							w.id, item.key, item.ctx.Err())
					}
				} else {
					// No context, execute directly with recovery
					item.ctx = context.Background() // Ensure context is set
					recovered, panicVal := w.safeExecute(&item)
					if recovered {
						w.logger.Errorf("Worker %d: Panic recovered for key: %s - %v",
							w.id, item.key, panicVal)
					} else {
						w.logger.Debugf("Worker %d: Completed item with key: %s (age: %v)",
							w.id, item.key, item.GetAge())
					}
				}
			}

		case <-w.stopSignal:
			// Drain remaining items in the queue before shutting down
			w.logger.Infof("Worker %d: Draining queue before shutdown", w.id)
			for {
				select {
				case item := <-w.queue:
					// Process remaining items if not expired/cancelled
					if !item.IsExpired() && !item.IsCancelled() && item.fn != nil {
						recovered, panicVal := w.safeExecute(&item) // <-- Use safeExecute
						if recovered {
							w.logger.Errorf("Worker %d: Panic recovered for key: %s - %v",
								w.id, item.key, panicVal)
						} else {
							w.logger.Debugf("Worker %d: Completed item with key: %s (age: %v)",
								w.id, item.key, item.GetAge())
						}
					}
				default:
					w.logger.Infof("Worker %d: Shutdown complete", w.id)
					return
				}
			}
		}
	}
}

// Enhanced execute with detailed recovery
func (w *Worker) safeExecute(item *QueueItem) (recovered bool, panicValue interface{}) {
	defer func() {
		if r := recover(); r != nil {
			recovered = true
			panicValue = r

			// Get stack trace
			stackTrace := debug.Stack()

			// Use custom logger if available
			if w.logger != nil {
				w.logger.Errorf("Worker %d: PANIC recovered for key '%s': %v", w.id, item.key, r)
			}
			w.logger.Debugf("Worker %d: Stack trace for key '%s':\n%s", w.id, item.key, string(stackTrace))
			// Call configured panic handler or default
			if w.panicHandler != nil {
				w.panicHandler(item, r, stackTrace)
			} else {
				DefaultPanicHandler(item, r, stackTrace)
			}
			// Store panic info in metadata
			if item.metadata == nil {
				item.metadata = make(map[string]interface{})
			}
			item.metadata["panic_recovered"] = true
			item.metadata["panic_value"] = fmt.Sprintf("%v", r)
			item.metadata["panic_time"] = time.Now()
			item.metadata["worker_id"] = w.id
			item.metadata["stack_trace"] = string(stackTrace)
		}
	}()

	// Execute the function
	w.logger.Debugf("Worker %d: Executing task with key: %s", w.id, item.key)
	// apply metadata to context if available
	if item.ctx != nil && item.metadata != nil {
		for k, v := range item.metadata {
			item.ctx = context.WithValue(item.ctx, MetadataKey(k), v)
		}
	}

	item.fn(item.ctx)
	return false, nil
}
