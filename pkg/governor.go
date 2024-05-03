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

// Governor oversees all services and manages shutdown
type Governor interface {
	Add(svc Service, name string)
	CatchSignals()
	Shutdown()
	WaitForShutdown()
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
	Log(fmt string, args ...any)
}

// Service is the minimum interface required for a service
type Service interface {
	// Run starts the service running.
	//
	// The Governor calls this on a new goroutine during the Add call.
	// When Run() returns, the service will be considered stopped.
	// The Governor also wraps this call with recover() and will
	// catch any panic() and log the error, then stop the service.
	//
	// Run() should use the api.Context() with any blocking
	// calls that take a Context, to support cancellation.
	// Network connections do not use a Context for reads and writes
	// (they do for Dial calls) so the service should implement the
	// Stoppable interface to close any open connections.
	// Use api.Sleep() to support cancellation.
	Run(api ServiceAPI)
}

// Stoppable allows a service to implement a Stop() handler
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
	//       is starting up (i.e. before Run() is called!)
	//
	// It can be used to e.g. close network sockets that the
	// service might be blocking on (in Go, blocking network
	// calls ignore Context cancellation, so this is necessary
	// to interrupt those calls.)
	Stop()
}

// StopNow is a sentinel value that can stop a service: panic(StopNow{})
type StopNow struct{}

type governor struct {
	wg       sync.WaitGroup     // used to wait for all services to call Stopped()
	ctx      context.Context    // root context for new services
	cancel   context.CancelFunc // used to cancel all service contexts
	mutex    sync.Mutex         // protects the following members:
	services []Service
	stopping bool
}

func NewGovernor() Governor {
	ctx, cancel := context.WithCancel(context.Background())
	g := &governor{
		ctx: ctx, cancel: cancel,
	}
	return g
}

// Add a new Service and start it running.
// This can be called from any goroutine.
func (g *governor) Add(svc Service, name string) {
	// protect against a race with Shutdown(), which can be
	// called from the CatchSignals goroutine (or any other)
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if g.stopping {
		log.Printf("[%s] cannot start while shutting down\n", name)
		return
	}
	g.services = append(g.services, svc)
	g.wg.Add(1)
	ctx, cancel := context.WithCancel(g.ctx)
	sctx := &service{ctx: ctx, cancel: cancel, wg: &g.wg, svc: svc, name: name}
	go func() {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("[%s] service crashed! error: %v\n", name, err)
			}
			log.Printf("[%s] service stopped.\n", name)
			g.wg.Done()
		}()
		log.Printf("[%s] service starting.\n", name)
		svc.Run(sctx)
	}()
}

// CatchSignals installs signal handlers for Ctrl+C and other stop signals.
// When a signal is received, Shutdown() is called.
func (g *governor) CatchSignals() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		<-signals
		log.Println("")
		log.Println("Shutdown requested via signal")
		g.Shutdown()
	}()
}

// Shutdown stops all services and waits for them to stop.
// This can be called from any goroutine.
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
	log.Println("Beginning shutdown...")
	g.cancel() // cancel all child contexts
	for _, svc := range g.services {
		// if the service has a Stop() function, call it.
		// this is for things like closing network sockets to interrupt blocked reads/writes.
		if stopper, ok := svc.(Stoppable); ok {
			stopper.Stop()
		}
	}
	g.wg.Wait()
	log.Println("Shutdown complete.")
}

// Wait for Governor to be shut down.
// This is useful at the end of main() to wait for shutdown.
// This can be called from any goroutine.
func (g *governor) WaitForShutdown() {
	g.wg.Wait()
}

// Service Context provides ServiceAPI for one service.
type service struct {
	ctx    context.Context // one per service (so we can stop/restart individual services)
	cancel context.CancelFunc
	wg     *sync.WaitGroup
	svc    Service
	name   string
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

func (c *service) Log(fmt string, args ...any) {
	fmt = fmtlib.Sprintf("[%s] %s\n", c.name, fmt)
	log.Printf(fmt, args...)
}
