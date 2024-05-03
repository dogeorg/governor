# Governor

Go Package `governor` provides a service management utility.

```go
import "github.com/raffecat/governor"
```

### Installing

```sh
go get github.com/raffecat/governor@v0.1.1
```

### Usage

Using Governor is meant to be simple:

```go
func main() {
    gov := governor.New().CatchSignals().Restart(1 * time.Second)

    gov.Add(serviceOne.New()).Name("service-one")

    if something {
        gov.Add(serviceTwo.New(args)).Name("service-two")
    }

    gov.Add(serviceThree.New(1)).Name("three-one")
    gov.Add(serviceThree.New(2)).Name("three-two")

    gov.Start()
    gov.WaitForShutdown()
}
```

In this example, four services are added to the governor.
All services are started together at the Start() call to allow channels
to be connected between them, etc. (services can be started individually.)

Services will restart if they crash due to the governer-level restart
policy (this can be overridden when adding each service, or removed entirely.)

Finally, at the end of main() we wait for all services to stop before exiting.
Since we used CatchSignals() it will call Shutdown() for us when SIGINT is received.


### New

New creates a new Governor to manage services.

```go
func New() Governor
```

### Governor interface

Governor oversees all services and manages shutdown.

```go
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
```

### Service interface

Service interface is the minimum that a service must implement.

```go
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
```

### ServiceAPI interface

ServiceAPI allows the service to call cancel-aware helpers from the servce
goroutine (or any child goroutine)

```go
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
```

### ServiceConfig interface

ServiceConfig allows you to configure a service when adding it to the
Governor.

```go
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
```

### StopService type

StopService is a sentinel value that can be used stop a service from deep
within a call stack by calling panic(StopService{}). Services are not
stopped until they return from their Run() function; this provides an escape
hatch for large, complex services.

```go
type StopService struct{}
```

### Stoppable interface

Stoppable allows a service to implement an optional Stop() handler.

```go
type Stoppable interface {

	// Stop is called to request the service to stop.
	//
	// This is called when the Governor is trying to stop the
	// service, after its Context is cancelled.
	//
	// NOTE: this will be called from a goroutine other than
	//       the service goroutine.
	//
	// NOTE: be aware that this can be called while your service
	//       is starting (i.e. around the time Run() is called!)
	//
	// Stop can be used to e.g. close network sockets that the
	// service might be blocking on (in Go, blocking network
	// calls ignore Context cancellation, so this is necessary
	// to interrupt those calls.)
	Stop()
}
```
