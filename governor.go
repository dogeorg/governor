// Package governor provides a service management utility.
package governor

import (
	"context"
	fmtlib "fmt"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Governor oversees all services and manages shutdown.
type Governor interface {

	// CatchSignals configures Governor to catch SIGINT (Ctrl+C)
	// and other interrupt signals (SIGTERM, SIGHUP, SIGQUIT)
	// and call Shutdown() when a signal is received.
	CatchSignals() Governor

	// Restart configures a default restart policy for all services,
	// which can be overridden on a per-service basis.
	// The restart delay can be zero.
	// Services never restart unless restart policy is set.
	Restart(after time.Duration) Governor

	// Add a service to Governor.
	// Returns a ServiceConfig interface to allow configuring and
	// starting the service.
	Add(svc Service) ServiceConfig

	// Start all added services.
	// Does not affect services that have already been started.
	Start()

	// Shutdown stops all services.
	// This call blocks until all services have stopped.
	Shutdown()

	// WaitForShutdown waits for all services to be stopped.
	// Caveat: will return immediately if no services have been started.
	// This can be useful at the end of main()
	WaitForShutdown()
}

// ServiceConfig allows you to configure a service when adding
// it to the Governor.
type ServiceConfig interface {

	// Configure the name of the service for errors and logging.
	Name(name string) ServiceConfig

	// Restart the service if it stops.
	// The restart delay can be zero.
	// This overrides the Governor restart policy.
	Restart(after time.Duration) ServiceConfig

	// Prevent service restart, to override Governor policy.
	// Services never restart unless a restart policy is set.
	NoRestart() ServiceConfig

	// Start the service immediately.
	// No more configuration is possible after calling Start()
	Start()
}

// ServiceAPI allows the service to call cancel-aware helpers
// from the servce goroutine (or any child goroutine)
type ServiceAPI interface {

	// Get the cancelable context for this service.
	// Pass this to blocking calls that take a Context.
	Context() context.Context

	// Sleep in a cancel-aware way.
	// The service should stop if this returns true.
	Sleep(duration time.Duration) (cancelled bool)

	// Log a message tagged with the service name.
	Log(fmt string, args ...interface{})
}

// Service interface is the minimum that a service must implement.
type Service interface {

	// Run is the main service goroutine.
	//
	// The Governor calls this on a new goroutine when the service
	// is started. When Run() returns, the service will be considered
	// stopped, and will be restarted if configured to do so.
	// The Governor also wraps Run() calls with recover() and will
	// catch any panic and log the error, then stop/restart the service.
	//
	// Run() should use the api.Context() with any blocking
	// calls that take a Context, to support cancellation.
	// Network connections do not use a Context for reads and writes
	// (they do for Dial calls) so the service should implement the
	// Stoppable interface to close any open net.Conn connections.
	// Use api.Sleep() instead of time.Sleep() to support cancellation.
	Run(api ServiceAPI)
}

// Stoppable allows a service to implement an optional Stop() handler.
type Stoppable interface {

	// Stop is called to request the service to stop.
	//
	// This is called when the Governor is trying to stop the
	// service, after its Context is cancelled.
	//
	// NOTE: this will be called from a goroutine other than
	//       the service goroutine (may need a sync.Mutex)
	//
	// NOTE: be aware that this can be called while your service
	//       is starting up (i.e. around the time Run() is called!)
	//
	// Stop can be used to e.g. close network sockets that the
	// service might be blocking on (in Go, blocking network
	// calls ignore Context cancellation, so this is necessary
	// to interrupt those calls.)
	Stop()
}

// StopService is a sentinel value that can be used stop a service
// from deep within a call stack by calling panic(StopService{}).
// Services are not stopped until they return from their Run() function;
// this provides an escape hatch for large, complex services.
type StopService struct{}

type governor struct {
	wg       sync.WaitGroup     // used to wait for all services to call Stopped()
	ctx      context.Context    // root context for new services
	cancel   context.CancelFunc // used to cancel all service contexts
	mutex    sync.Mutex         // protects the following members:
	services []*service
	delay    time.Duration
	stopping bool
	restart  bool
}

// New creates a new Governor to manage services.
func New() Governor {
	ctx, cancel := context.WithCancel(context.Background())
	g := &governor{
		ctx: ctx, cancel: cancel,
	}
	return g
}

func (g *governor) CatchSignals() Governor {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		<-signals
		log.Println("")
		log.Println("Shutdown requested via signal")
		g.Shutdown()
	}()
	return g
}

func (g *governor) Restart(after time.Duration) Governor {
	g.delay = after
	g.restart = true
	return g
}

func (g *governor) Add(svc Service) ServiceConfig {
	// protect against a race with Shutdown
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if g.stopping {
		return &service{}
	}
	sctx := &service{g: g, svc: svc, delay: g.delay, restart: g.restart}
	g.services = append(g.services, sctx)
	return sctx
}

func (g *governor) Start() {
	// protect against a race with Shutdown
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if g.stopping {
		return
	}
	for _, svc := range g.services {
		svc.Start()
	}
}

func (g *governor) Shutdown() {
	// protect against a race with CatchSignals() goroutine
	g.mutex.Lock()
	if g.stopping {
		g.mutex.Unlock()
		return
	}
	g.stopping = true
	g.mutex.Unlock()
	// no need to hold the mutex below (Add calls are blocked)
	log.Println("Beginning shutdown…")
	g.cancel() // cancel all child contexts
	for _, svc := range g.services {
		// if the service has a Stop() function, call it
		if stopper, ok := svc.svc.(Stoppable); ok {
			stopper.Stop()
		}
	}
	g.wg.Wait()
	log.Println("Shutdown complete.")
}

func (g *governor) WaitForShutdown() {
	g.wg.Wait()
}

func (g *governor) is_stopping() bool {
	// protect against a race with Shutdown
	g.mutex.Lock()
	defer g.mutex.Unlock()
	return g.stopping
}

// Service Context provides ServiceAPI for one service.
type service struct {
	g       *governor
	ctx     context.Context // one per service (so we can stop/restart individual services)
	cancel  context.CancelFunc
	svc     Service
	name    string
	delay   time.Duration
	restart bool
}

func (c *service) Name(name string) ServiceConfig {
	c.name = name
	return c
}

func (c *service) Restart(after time.Duration) ServiceConfig {
	c.delay = after
	c.restart = true
	return c
}

func (c *service) NoRestart() ServiceConfig {
	c.restart = false
	return c
}

func (c *service) Start() {
	if c.ctx != nil {
		return // already running
	}
	g := c.g
	g.wg.Add(1)
	c.ctx, c.cancel = context.WithCancel(g.ctx)
	go func() {
		for {
			c.service_run()
			if !g.is_stopping() && c.restart {
				log.Printf("[%s] service stopped, restarting in %v\n", c.name, c.delay)
				c.Sleep(c.delay)
			} else {
				log.Printf("[%s] service stopped.\n", c.name)
				g.wg.Done()
				return
			}
		}
	}()
}

func (c *service) service_run() {
	defer func() {
		if err := recover(); err != nil {
			// don't log if the panic value is StopImmediate{}
			if _, ok := err.(StopService); ok {
				log.Printf("[%s] raised stop-immediate.\n", c.name)
			} else {
				log.Printf("[%s] crashed! error: %v\n", c.name, err)
			}
		}
	}()
	log.Printf("[%s] service starting.\n", c.name)
	c.svc.Run(c)
}

func (c *service) Context() context.Context { return c.ctx }

func (c *service) Sleep(duration time.Duration) (cancelled bool) {
	select {
	case <-c.ctx.Done(): // receive context cancel
		return true
	case <-time.After(duration):
		return false
	}
}

func (c *service) Log(fmt string, args ...interface{}) {
	fmt = fmtlib.Sprintf("[%s] %s\n", c.name, fmt)
	log.Printf(fmt, args...)
}
