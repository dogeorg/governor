package governor

import (
	"context"
	fmtlib "fmt"
	"log"
	"sync"
	"time"
)

// Service interface are the methods a service must implement.
// The service should also embed `ServiceCtx` in the service struct.
type Service interface {

	// Run is the main service goroutine.
	//
	// The Governor calls this on a new goroutine when the service
	// is started. When Run() returns, the service will stop
	// and will be restarted if configured to do so.
	// The Governor wraps Run() calls with recover() and will catch
	// any panic() and log the error, then stop/restart the service.
	//
	// Run() should use the ServiceCtx.Ctx with any blocking
	// calls that can take a Context, to support cancellation.
	//
	// Network connections do not use a Context for reads and writes
	// (but they do for Dial calls) so the service should implement
	// the Stoppable interface to close any open net.Conn connections.
	//
	// Use ServiceCtx.Sleep() instead of time.Sleep() to support cancellation.
	Run()
}

type Stoppable interface {

	// Stop is called to stop the service (e.g. during shutdown)
	//
	// This is called when the Governor is trying to stop the
	// service, after the service Context is cancelled.
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

// ServiceCtx is embedded in all service implementation-structs
// to provide a Context and common helper functions.
type ServiceCtx struct {
	ServiceName string
	Context     context.Context

	// readiness barrier, closed when the service is ready
	readyCh   chan struct{}
	readyOnce sync.Once
}

// private interface to allow governor to init the ServiceCtx
type governorServiceCtxInit interface {
	initGovernorServiceCtx(ctx context.Context, name string)
}

func (s *ServiceCtx) initGovernorServiceCtx(ctx context.Context, name string) {
	s.Context = ctx
	s.ServiceName = name
	// create a fresh readiness channel for each start/restart
	s.readyCh = make(chan struct{})
	s.readyOnce = sync.Once{}
}

// Stopping returns true if the servce has been asked to stop:
// caller should exit the goroutine.
func (s *ServiceCtx) Stopping() bool {
	return s.Context.Err() != nil // Canceled or DeadlineExceeded
}

// Sleep returns true if it was interrupted by Governor asking
// the service to stop: caller should exit the service goroutine.
func (s *ServiceCtx) Sleep(duration time.Duration) bool {
	select {
	case <-s.Context.Done(): // Canceled or DeadlineExceeded
		return true
	case <-time.After(duration):
		return false
	}
}

// Log a message tagged with the service name.
func (s *ServiceCtx) Log(fmt string, args ...interface{}) {
	fmt = fmtlib.Sprintf("[%s] %s\n", s.ServiceName, fmt)
	log.Printf(fmt, args...)
}

// MarkReady signals that the service has finished its startup and is healthy.
func (s *ServiceCtx) MarkReady() {
	if s.readyCh == nil {
		// ensure channel exists in case service uses ServiceCtx without init hook
		s.readyCh = make(chan struct{})
	}
	s.readyOnce.Do(func() {
		close(s.readyCh)
	})
}

// Ready returns a channel that is closed when the service is ready.
func (s *ServiceCtx) Ready() <-chan struct{} {
	return s.readyCh
}

// WaitForReady blocks until the service is ready, context is cancelled,
// or the timeout elapses. If timeout <= 0, it waits indefinitely until
// ready or cancelled. Returns true if ready, false if cancelled or timed out.
func (s *ServiceCtx) WaitForReady(timeout time.Duration) bool {
	if s.readyCh == nil {
		// if not initialized yet, create to avoid nil channel blocking forever
		s.readyCh = make(chan struct{})
	}
	if timeout <= 0 {
		select {
		case <-s.readyCh:
			return true
		case <-s.Context.Done():
			return false
		}
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.readyCh:
		return true
	case <-s.Context.Done():
		return false
	case <-timer.C:
		return false
	}
}
