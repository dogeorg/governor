package governor_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dogeorg/governor"
	"gotest.tools/v3/assert"
)

var mu sync.Mutex

type FakeStore struct {
	governor.ServiceCtx
	LocalRunning chan string
}

func (fs *FakeStore) Run() {
	fmt.Println("Starting... FakeStore")
	time.Sleep(1 * time.Second)

	fs.MarkReady()
	mu.Lock()
	defer mu.Unlock()
	fs.LocalRunning <- "store"
	fmt.Println("Started FakeStore")
}

type FakeApp struct {
	Name         string
	LocalRunning chan string
	governor.ServiceCtx
}

func (fs *FakeApp) Run() {
	fmt.Println("Starting... FakeApp")
	fs.MarkReady()
	mu.Lock()
	defer mu.Unlock()
	fs.LocalRunning <- fs.Name
	fmt.Println("Started FakeApp")
}

func TestGovernor(t *testing.T) {
	running := make(chan string)
	gov := governor.New()

	gov.Add("app2", &FakeApp{Name: "app2", LocalRunning: running})
	gov.Add("app", &FakeApp{Name: "app", LocalRunning: running})
	gov.Add("store", &FakeStore{LocalRunning: running})

	gov.StartWaitReady(3 * time.Minute)

	assert.Equal(t, <-running, "app2")
	assert.Equal(t, <-running, "app")
	assert.Equal(t, <-running, "store")

	gov.WaitForShutdown()
}

func TestGovernorWithReady(t *testing.T) {
	running := make(chan string)
	gov := governor.New()

	gov.Add("app2", &FakeApp{Name: "app2", LocalRunning: running}).DependsOn("store", "app")
	gov.Add("app", &FakeApp{Name: "app", LocalRunning: running}).DependsOn("store")
	gov.Add("store", &FakeStore{LocalRunning: running})

	gov.StartWaitReady(3 * time.Minute)

	assert.Equal(t, <-running, "store")
	assert.Equal(t, <-running, "app")
	assert.Equal(t, <-running, "app2")

	gov.WaitForShutdown()
}
