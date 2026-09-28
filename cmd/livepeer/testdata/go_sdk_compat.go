package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"

	liverunner "github.com/livepeer/golang-runner"
)

func main() {
	if len(os.Args) != 4 {
		panic("usage: go_sdk_compat <orchestrator> <runner> <bootstrap>")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	reserved := make(chan string, 1)
	released := make(chan string, 1)
	r, err := liverunner.Register(ctx, liverunner.Options{
		OrchestratorURL: os.Args[1], RunnerURL: os.Args[2], BootstrapSecret: os.Args[3],
		App: "sdk-compat", Mode: liverunner.ModePersistent, Capacity: 1,
		OnReserve: func(ev liverunner.SessionEvent) { reserved <- ev.SessionID },
		OnRelease: func(ev liverunner.SessionEvent) { released <- ev.SessionID },
	})
	if err != nil {
		panic(err)
	}
	defer func() { _ = r.Close(context.Background()) }()
	response, err := http.Post(os.Args[1]+"/apps/"+r.RunnerID()+"/session", "application/json", nil)
	if err != nil {
		panic(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		panic(fmt.Sprintf("reservation status %d", response.StatusCode))
	}
	var session struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&session); err != nil {
		panic(err)
	}
	select {
	case got := <-reserved:
		if got != session.SessionID {
			panic("wrong reserved session")
		}
	case <-ctx.Done():
		panic("no SDK reserve callback")
	}
	stop, err := http.Post(os.Args[1]+"/apps/"+r.RunnerID()+"/session/"+session.SessionID+"/stop", "application/json", nil)
	if err != nil {
		panic(err)
	}
	_ = stop.Body.Close()
	if stop.StatusCode != http.StatusOK {
		panic(fmt.Sprintf("stop status %d", stop.StatusCode))
	}
	select {
	case got := <-released:
		if got != session.SessionID {
			panic("wrong released session")
		}
	case <-ctx.Done():
		panic("no SDK release callback")
	}
	fmt.Println("Go SDK registration, reserve/release callbacks and unregister passed")
}
