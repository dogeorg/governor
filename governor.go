package governor

import (
	"context"
	"fmt"
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
	Add(name string, svc Service) ServiceConfig

	// Start all added services.
	// Does not affect services that have already been started.
	// Services with dependencies will be started after their dependencies.
	Start() Governor

	// Start all added services, then wait for all services that expose Ready()
	// to become ready or until timeout elapses. If timeout <= 0, wait indefinitely.
	StartWaitReady(timeout time.Duration) Governor

	// WaitReady waits for a specific service's Ready() to close or until timeout.
	// If the service does not expose Ready(), this returns true immediately.
	WaitReady(name string, timeout time.Duration) bool

	// Shutdown stops all services.
	// This call blocks until all services have stopped.
	Shutdown()

	// WaitForShutdown waits for all services to be stopped.
	// Caveat: will return immediately if no services have been started.
	// This can be useful at the end of main()
	WaitForShutdown()

	// GlobalContext gets a cancellable context for use outside of any service.
	// This context will be cancelled when shutdown is requested.
	GlobalContext() context.Context

	// Sleep blocks for 'duration', cancelled if GlobalContext() is cancelled.
	Sleep(duration time.Duration) (cancelled bool)

	// Stopping returns true if GlobalContext() is cancelled.
	Stopping() bool
}

// ServiceConfig allows you to configure a service when adding
// it to the Governor.
type ServiceConfig interface {

	// Name sets or changes the service name.
	// Renaming after Add() updates the internal name index used for dependencies.
	Name(name string) ServiceConfig

	// Declare dependencies by service name. When Start() is called,
	// the Governor ensures that dependencies are started first.
	// Dependencies can be added before the dependency service is added;
	// validation occurs at Start().
	DependsOn(names ...string) ServiceConfig

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
	// servicesByName indexes services by current name and is kept updated
	// if a service is renamed via ServiceConfig.Name().
	servicesByName map[string]*service
	delay          time.Duration
	stopping       bool
	restart        bool
}

// New creates a new Governor to manage services.
func New() Governor {
	ctx, cancel := context.WithCancel(context.Background())
	g := &governor{
		ctx:            ctx,
		cancel:         cancel,
		servicesByName: make(map[string]*service),
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

func (g *governor) Add(name string, svc Service) ServiceConfig {
	// protect against a race with Shutdown
	g.mutex.Lock()
	defer g.mutex.Unlock()
	if g.stopping {
		return &service{}
	}
	if _, exists := g.servicesByName[name]; exists {
		panic(fmt.Sprintf("duplicate service name: %q", name))
	}
	sctx := &service{
		g:         g,
		svc:       svc,
		delay:     g.delay,
		restart:   g.restart,
		name:      name,
		startedCh: make(chan struct{}),
	}
	g.services = append(g.services, sctx)
	g.servicesByName[name] = sctx
	return sctx
}

func (g *governor) Start() Governor {
	// protect against a race with Shutdown
	g.mutex.Lock()
	if g.stopping {
		g.mutex.Unlock()
		return g
	}

	// Determine which services are not yet started (pending).
	pending := make([]*service, 0, len(g.services))
	for _, svc := range g.services {
		if svc.ctx == nil {
			pending = append(pending, svc)
		}
	}
	if len(pending) == 0 {
		return g
	}

	// Build a dependency graph among pending services only.
	// If a dependency is already running, it doesn't contribute to indegree.
	type void struct{}
	_ = void{}
	indegree := make(map[*service]int, len(pending))
	adj := make(map[*service][]*service, len(pending))

	for _, s := range pending {
		indegree[s] = 0
	}

	for _, s := range pending {
		for _, depName := range s.deps {
			depSvc, ok := g.servicesByName[depName]
			if !ok {
				panic(fmt.Sprintf("[%s] depends on unknown service %q", s.name, depName))
			}
			// If the dependency isn't pending (already running), no edge is needed.
			if depSvc.ctx == nil {
				adj[depSvc] = append(adj[depSvc], s)
				indegree[s]++
			}
		}
	}

	// Kahn's algorithm for topological sort with stable ordering:
	// seed queue with zero-indegree nodes in insertion order.
	queue := make([]*service, 0, len(pending))
	inQueue := make(map[*service]bool, len(pending))
	for _, s := range pending {
		if indegree[s] == 0 {
			queue = append(queue, s)
			inQueue[s] = true
		}
	}

	order := make([]*service, 0, len(pending))
	for len(queue) > 0 {
		n := queue[0]
		queue = queue[1:]
		order = append(order, n)
		for _, m := range adj[n] {
			indegree[m]--
			if indegree[m] == 0 && !inQueue[m] {
				queue = append(queue, m)
				inQueue[m] = true
			}
		}
	}

	if len(order) != len(pending) {
		// There is a dependency cycle among services we need to start.
		cycled := make([]string, 0, len(pending))
		for _, s := range pending {
			if indegree[s] > 0 {
				cycled = append(cycled, s.name)
			}
		}
		panic(fmt.Sprintf("dependency cycle detected among services being started: %v", cycled))
	}

	// Precompute dependencies under lock, then release the lock before waiting/starting.
	depsBySvc := make(map[*service][]*service, len(order))
	for _, s := range order {
		for _, depName := range s.deps {
			if depSvc, ok := g.servicesByName[depName]; ok {
				depsBySvc[s] = append(depsBySvc[s], depSvc)
			} else {
				// should not happen; already validated above
				panic(fmt.Sprintf("[%s] depends on unknown service %q", s.name, depName))
			}
		}
	}
	// release governor mutex before any waits
	g.mutex.Unlock()

	// Start services in dependency order.
	// For each service, wait until all of its dependencies are ready (if supported),
	// otherwise at least started, before starting the dependent.
	for _, svc := range order {
		// Wait for dependencies to be ready/started
		for _, depSvc := range depsBySvc[svc] {
			// Wait for dependency to have started
			if depSvc.startedCh != nil {
				select {
				case <-depSvc.startedCh:
				case <-g.ctx.Done():
					return g
				}
			}
			// If dependency exposes readiness, wait for it to be ready
			type hasReady interface{ Ready() <-chan struct{} }
			if rc, ok := depSvc.svc.(hasReady); ok {
				if ch := rc.Ready(); ch != nil {
					select {
					case <-ch:
					case <-g.ctx.Done():
						return g
					}
				}
			}
		}

		// Start this service now and wait until its goroutine has entered.
		svc.Start()
		if svc.startedCh != nil {
			select {
			case <-svc.startedCh:
			case <-g.ctx.Done():
				return g
			}
		}
	}

	return g
}

func (g *governor) StartWaitReady(timeout time.Duration) Governor {
	// Ensure services are started in dependency order.
	g.Start()

	// Collect readiness channels from started services.
	type hasReady interface {
		Ready() <-chan struct{}
	}

	g.mutex.Lock()
	chans := make([]<-chan struct{}, 0, len(g.services))
	for _, s := range g.services {
		if s.ctx != nil {
			if rc, ok := s.svc.(hasReady); ok {
				if ch := rc.Ready(); ch != nil {
					chans = append(chans, ch)
				}
			}
		}
	}
	g.mutex.Unlock()

	if len(chans) == 0 {
		return g
	}

	ctx, cancel := context.WithCancel(g.ctx)
	defer cancel()

	readyOne := make(chan struct{}, len(chans))
	for _, ch := range chans {
		go func(ch <-chan struct{}) {
			select {
			case <-ch:
				readyOne <- struct{}{}
			case <-ctx.Done():
				return
			}
		}(ch)
	}

	var timeoutC <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timeoutC = t.C
	}

	readyCount := 0
	for readyCount < len(chans) {
		select {
		case <-readyOne:
			readyCount++
		case <-timeoutC:
			cancel()
			return g
		case <-g.ctx.Done():
			cancel()
			return g
		}
	}

	return g
}

func (g *governor) WaitReady(name string, timeout time.Duration) bool {
	type hasReady interface {
		Ready() <-chan struct{}
	}

	g.mutex.Lock()
	s := g.servicesByName[name]
	g.mutex.Unlock()
	if s == nil {
		return false
	}

	// Phase 1: wait for the service to start (startedCh closes on start)
	startCh := s.startedCh
	if timeout <= 0 {
		select {
		case <-startCh:
			// proceed to readiness
		case <-g.ctx.Done():
			return false
		}
	} else {
		t1 := time.NewTimer(timeout)
		defer t1.Stop()
		start := time.Now()
		select {
		case <-startCh:
			// proceed
		case <-g.ctx.Done():
			return false
		case <-t1.C:
			return false
		}
		// compute remaining time for readiness wait
		timeout -= time.Since(start)
		if timeout <= 0 {
			return true // started, but no time left to wait for readiness
		}
	}

	// Phase 2: wait for readiness if supported
	rc, ok := s.svc.(hasReady)
	if !ok {
		return true
	}
	ch := rc.Ready()
	if ch == nil {
		return true
	}

	if timeout <= 0 {
		select {
		case <-ch:
			return true
		case <-g.ctx.Done():
			return false
		}
	}

	t2 := time.NewTimer(timeout)
	defer t2.Stop()
	select {
	case <-ch:
		return true
	case <-g.ctx.Done():
		return false
	case <-t2.C:
		return false
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

func (g *governor) GlobalContext() context.Context {
	return g.ctx
}

func (g *governor) Sleep(duration time.Duration) (cancelled bool) {
	select {
	case <-g.ctx.Done(): // receive context cancel
		return true
	case <-time.After(duration):
		return false
	}
}

func (g *governor) Stopping() bool {
	return g.ctx.Err() != nil // cancelled
}

func (g *governor) is_stopping() bool {
	// protect against a race with Shutdown
	g.mutex.Lock()
	defer g.mutex.Unlock()
	return g.stopping
}

// Service Context provides ServiceAPI for one service.
type service struct {
	g           *governor
	ctx         context.Context // one per service (so we can stop/restart individual services)
	cancel      context.CancelFunc
	svc         Service
	name        string
	delay       time.Duration
	restart     bool
	deps        []string      // list of service names this service depends on
	startedCh   chan struct{} // closed once when the service goroutine enters service_entry
	startedOnce sync.Once
}

func (c *service) Name(name string) ServiceConfig {
	g := c.g
	// Update the name and keep the governor's name index in sync.
	if g == nil {
		c.name = name
		return c
	}

	g.mutex.Lock()
	defer g.mutex.Unlock()

	if c.name == name {
		return c
	}
	if _, exists := g.servicesByName[name]; exists {
		panic(fmt.Sprintf("duplicate service name: %q", name))
	}

	// Remove old entry and add new one in the index.
	delete(g.servicesByName, c.name)
	c.name = name
	g.servicesByName[name] = c
	return c
}

func (c *service) DependsOn(names ...string) ServiceConfig {
	c.deps = append(c.deps, names...)
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
	if c.startedCh == nil {
		c.startedCh = make(chan struct{})
	}
	c.ctx, c.cancel = context.WithCancel(g.ctx)
	go c.service_goroutine(g)
}

func (c *service) service_goroutine(g *governor) {
	// keep restarting the service until shutdown.
	for {
		c.service_entry()
		if !g.is_stopping() && c.restart {
			log.Printf("[%s] service stopped, restarting in %v\n", c.name, c.delay)
			c.sleep(c.delay)
		} else {
			log.Printf("[%s] service stopped.\n", c.name)
			g.wg.Done()
			return
		}
	}
}

func (c *service) service_entry() {
	// run the service until it returns or panics, or until shutdown.
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
	// signal started barrier as soon as goroutine enters service
	c.startedOnce.Do(func() {
		if c.startedCh != nil {
			close(c.startedCh)
		}
	})
	if init, ok := c.svc.(governorServiceCtxInit); ok {
		init.initGovernorServiceCtx(c.ctx, c.name)
	} else {
		panic(fmt.Sprintf("[%s] bug: service does not embed governor.ServiceCtx", c.name))
	}
	c.svc.Run()
}

func (c *service) sleep(duration time.Duration) (cancelled bool) {
	select {
	case <-c.ctx.Done(): // receive context cancel
		return true
	case <-time.After(duration):
		return false
	}
}
