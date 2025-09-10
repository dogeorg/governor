package governor_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/dogeorg/governor"
	"gotest.tools/v3/assert"
)

var running []string = []string{}
var mu sync.Mutex

type FakeStore struct {
	governor.ServiceCtx
}

func (fs *FakeStore) Run() {
	fmt.Println("Starting... FakeStore")
	time.Sleep(1 * time.Second)

	fs.MarkReady()
	mu.Lock()
	defer mu.Unlock()
	running = append(running, "store")
	fmt.Println("Started FakeStore")
}

func (fs *FakeStore) Stop() {

}

type FakeApp struct {
	Name string
	governor.ServiceCtx
}

func (fs *FakeApp) Run() {
	fmt.Println("Starting... FakeApp")
	fs.MarkReady()
	mu.Lock()
	defer mu.Unlock()
	running = append(running, fs.Name)
	fmt.Println("Started FakeApp")
}

func (fs *FakeApp) Stop() {

}

func TestGovernor(t *testing.T) {
	gov := governor.New()

	gov.Add("app2", &FakeApp{Name: "app2"}).DependsOn("store", "app")
	gov.Add("app", &FakeApp{Name: "app"}).DependsOn("store")
	gov.Add("store", &FakeStore{})

	gov.StartWaitReady(3 * time.Minute)

	for {
		if len(running) == 3 {
			break
		}

		time.Sleep(1 * time.Second)
	}

	assert.Equal(t, running[0], "store")
	assert.Equal(t, running[1], "app")
	assert.Equal(t, running[2], "app2")

	gov.WaitForShutdown()
}
