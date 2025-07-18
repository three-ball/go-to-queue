// Package gotoqueue provides a high-performance, key-based worker queue implementation
// with FIFO ordering and concurrent processing capabilities.
//
// The library allows you to enqueue tasks with string keys, ensuring that tasks
// with the same key are processed sequentially by the same worker, while different
// keys can be processed concurrently by different workers.
//
// Key features:
//   - Key-based routing: Items with same key go to same worker
//   - Concurrent workers: Multiple workers process different keys in parallel
//   - Context support: Timeout and cancellation handling
//   - Expiration: Items can expire before processing
//   - Metadata: Attach custom data to queue items
//   - Thread-safe: Safe for concurrent use across multiple goroutines
//
// Example usage:
//
//	pool := gotoqueue.NewPool(3, 100) // 3 workers, 100 buffer per worker
//	pool.Start()
//	defer pool.Stop()
//
//	// Basic enqueue
//	pool.Enqueue("user:123", func(ctx context.Context) {
//		fmt.Println("Processing user 123")
//	})
//
//	// With timeout
//	pool.Enqueue("order:456", func(ctx context.Context) {
//		fmt.Println("Processing order 456")
//	}, gotoqueue.WithTimeout(5*time.Second))
package gotoqueue

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spaolacci/murmur3"
)

type Strategy int

const (
	KeyBased   Strategy = iota // Key-based routing: items with same key go to same worker
	RoundRobin                 // Round-robin: items are distributed evenly across workers
)

// Pool represents a pool of workers that can process items from the queue.
type Pool struct {
	strategy         Strategy       // Strategy for distributing items to workers
	roundRobinIndex  int64          // Atomic index for round-robin strategy
	workers          []*Worker      // Slice of workers in the pool
	size             int            // Number of workers in the pool
	wg               sync.WaitGroup // WaitGroup to wait for all workers to finish processing
	mutex            sync.Mutex     // Mutex to protect access to the pool state
	isRunning        bool           // Indicates if the pool is currently running
	logger           Logger         // Pool-level logger
	logLevel         LogLevel       // Current log level
	shutdownSignal   chan os.Signal // Channel to handle OS signals for graceful shutdown
	gracefulShutdown bool           // Flag to indicate if the pool should shut down gracefully
}

// to calculates the index of the worker based on the key.
func (p *Pool) to(k string) int {
	switch p.strategy {
	case RoundRobin:
		// Use atomic operations for thread-safe round-robin
		index := atomic.AddInt64(&p.roundRobinIndex, 1)
		return int(index-1) % p.size
	default:
		hash := murmur3.Sum32([]byte(k))
		return int(hash) % p.size
	}
}

// NewPool creates a new worker pool with the specified size and buffer size.
// The pool size determines how many workers will be created, and the buffer size determines how many items can be buffered in each worker's queue.
// default of poolSize is 1 and bufferSize is 100 if they are not provided or are less than or equal to zero.
func NewPool(poolSize int, bufferSize int, s Strategy) *Pool {
	if s != KeyBased && s != RoundRobin {
		log.Printf("Invalid strategy %d, defaulting to KeyBased", s)
		s = KeyBased // Default to KeyBased if invalid strategy is provided
	}

	if poolSize <= 0 {
		poolSize = 1 // Ensure at least one worker
	}

	if bufferSize <= 0 {
		bufferSize = 100 // Ensure at least one item can be buffered
	}

	logger := NewDefaultLogger(LogLevelInfo)

	pool := &Pool{
		size:             poolSize,
		workers:          make([]*Worker, poolSize),
		isRunning:        false,
		strategy:         s,
		logger:           logger,
		logLevel:         LogLevelSilent,
		gracefulShutdown: false,
	}

	for i := 0; i < poolSize; i++ {
		pool.workers[i] = &Worker{
			id:           i,
			queue:        make(chan QueueItem, bufferSize),
			stopSignal:   make(chan struct{}),
			wg:           &pool.wg,
			logger:       logger,              // Share pool logger with workers
			panicHandler: DefaultPanicHandler, // Use default panic handler
		}
	}

	return pool
}

// handleShutdownSignal listens for shutdown signals and initiates graceful shutdown
func (p *Pool) handleShutdownSignal() {
	<-p.shutdownSignal
	p.logger.Infof("Received shutdown signal, starting graceful shutdown...")
	p.gracefulShutdown = true
	p.Stop()
}

func (p *Pool) SetPanicHandler(handler PanicHandler) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	for _, worker := range p.workers {
		worker.panicHandler = handler
	}
}

// SetLogLevel sets the log level for the pool and all workers
func (p *Pool) SetLogLevel(level string) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	p.logLevel = ParseLogLevel(level)

	// Update pool logger
	if p.logger != nil {
		p.logger.SetLevel(p.logLevel)
	}

	// Update all worker loggers
	for _, worker := range p.workers {
		if worker.logger != nil {
			worker.logger.SetLevel(p.logLevel)
		}
	}
}

// GetLogLevel returns the current log level
func (p *Pool) GetLogLevel() LogLevel {
	p.mutex.Lock()
	defer p.mutex.Unlock()
	return p.logLevel
}

// SetLogger sets a custom logger for the pool and all workers
func (p *Pool) SetLogger(logger Logger) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	p.logger = logger

	// Update all worker loggers
	for _, worker := range p.workers {
		worker.logger = logger
	}
}

// Start starts the worker pool.
// It initializes the workers and starts them in goroutines.
// If the pool is already running, it does nothing.
func (p *Pool) Start() {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	// Check if the pool is already running
	if p.isRunning {
		return
	}

	p.isRunning = true

	p.shutdownSignal = make(chan os.Signal, 1)
	signal.Notify(p.shutdownSignal, syscall.SIGINT, syscall.SIGTERM)

	// Start all workers
	for i := 0; i < p.size; i++ {
		p.wg.Add(1)
		go p.workers[i].start()
	}

	// Start a goroutine to handle shutdown signals
	go p.handleShutdownSignal()
}

// Stop stops the worker pool.
// It signals all workers to stop and waits for them to finish processing.
// If the pool is already stopped, it does nothing.
// It also closes all queues to prevent further enqueuing of items.
func (p *Pool) Stop() {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	// Check if the pool is already stopped
	if !p.isRunning {
		return
	}

	p.isRunning = false

	// Signal all workers to stop
	for _, worker := range p.workers {
		close(worker.stopSignal)
	}

	// Wait for all workers to finish processing
	p.wg.Wait()

	// Close all queues
	for i := 0; i < p.size; i++ {
		close(p.workers[i].queue)
	}
}

// Enqueue adds a new item to the queue with optional configuration.
// This is the main method that uses the options pattern.
// It returns the worker index where the item was enqueued, or an error if the operation failed.
func (p *Pool) Enqueue(key string, fn func(context.Context), opts ...EnqueueOption) (int, error) {
	// Apply all options
	options := applyEnqueueOptions(opts...)

	p.mutex.Lock()
	isRunning := p.isRunning
	p.mutex.Unlock()

	// Check if the pool is running
	if !isRunning {
		return 0, ErrQueueNotRunning
	}

	// Check if already expired
	if !options.expireTime.IsZero() && time.Now().After(options.expireTime) {
		return 0, ErrQueueItemExpired
	}

	// Check if context is already cancelled
	if options.itemCtx != nil {
		select {
		case <-options.itemCtx.Done():
			return 0, ErrQueueItemCancelled
		default:
		}
	}

	// Calculate the worker index based on the key
	workerIndex := p.to(key)

	// Copy metadata to avoid external modifications
	var itemMetadata map[string]interface{}
	if options.metadata != nil {
		itemMetadata = make(map[string]interface{})
		for k, v := range options.metadata {
			itemMetadata[k] = v
		}
	}

	item := QueueItem{
		key:         key,
		fn:          fn,
		ctx:         options.itemCtx,
		metadata:    itemMetadata,
		enqueueTime: time.Now(),
		expireTime:  options.expireTime,
	}

	p.workers[workerIndex].queue <- item
	return workerIndex, nil
}

// GetQueueLength returns the length of the queue for a specific worker by its ID.
// It returns an error if the worker ID is invalid.
// The worker ID should be between 0 and size-1.
func (p *Pool) GetQueueLength(id int) (int, error) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	if id < 0 || id >= p.size {
		return 0, ErrInvalidWorkerID
	}

	return len(p.workers[id].queue), nil
}

// GetTotalQueueLength returns the total number of items in all workers' queues.
func (p *Pool) GetTotalQueueLength() int {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	length := 0
	for _, worker := range p.workers {
		length += len(worker.queue)
	}
	return length
}

// GetPoolSize returns the number of workers in the pool.
// It returns the size of the pool, which is the number of workers.
func (p *Pool) GetPoolSize() int {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	return p.size
}

// IsRunning checks if the worker pool is currently running.
// It returns true if the pool is running, false otherwise.
func (p *Pool) IsRunning() bool {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	return p.isRunning
}

// GetStrategy returns the current strategy used by the pool.
func (p *Pool) GetStrategy() Strategy {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	return p.strategy
}
