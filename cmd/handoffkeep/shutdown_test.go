package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mgh3326/handoffkeep/internal/store"
)

func shutdownTestStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("HANDOFFKEEP_TEST_DB_URL")
	if url == "" {
		t.Skip("HANDOFFKEEP_TEST_DB_URL is required for shutdown tests")
	}
	st, err := store.Open(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	return st
}

func waitForRefusedConnection(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", address, 20*time.Millisecond)
		if err != nil {
			return
		}
		_ = conn.Close()
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("listener continued accepting connections after shutdown began")
}

func waitForGoroutineBaseline(t *testing.T, baseline int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		if got := runtime.NumGoroutine(); got <= baseline {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	stack := make([]byte, 1<<20)
	n := runtime.Stack(stack, true)
	t.Fatalf("goroutines did not settle: got=%d baseline=%d\n%s", runtime.NumGoroutine(), baseline, stack[:n])
}

func TestRunServerGracefulShutdown(t *testing.T) {
	st := shutdownTestStore(t)
	// os/signal starts one process-global receiver on first use and keeps it
	// for the process lifetime. Prime it before measuring server goroutines.
	primed, stopPriming := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	stopPriming()
	<-primed.Done()
	baseline := runtime.NumGoroutine()
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-releaseRequest
		if _, err := st.ListTasks(r.Context(), "", "", "", 1); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "drained")
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runServer(ctx, serveOptions{bindings: []serverBinding{{server: server, listener: listener}}})
	}()

	transport := &http.Transport{DisableKeepAlives: true}
	client := &http.Client{Transport: transport, Timeout: 2 * time.Second}
	response := make(chan *http.Response, 1)
	requestErr := make(chan error, 1)
	go func() {
		resp, err := client.Get("http://" + listener.Addr().String())
		if err != nil {
			requestErr <- err
			return
		}
		response <- resp
	}()
	select {
	case <-requestStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not reach handler")
	}

	cancel()
	waitForRefusedConnection(t, listener.Addr().String())
	close(releaseRequest)
	select {
	case err := <-requestErr:
		t.Fatalf("in-flight request failed: %v", err)
	case resp := <-response:
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			t.Fatalf("in-flight status=%d", resp.StatusCode)
		}
		body, err := io.ReadAll(resp.Body)
		closeErr := resp.Body.Close()
		if err != nil || string(body) != "drained" {
			t.Fatalf("body=%q err=%v", body, err)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("in-flight request did not finish")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	transport.CloseIdleConnections()
	waitForGoroutineBaseline(t, baseline)
}

func TestRunServerShutdownBudget(t *testing.T) {
	requestStarted := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-r.Context().Done()
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: handler}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- runServer(ctx, serveOptions{
			bindings:         []serverBinding{{server: server, listener: listener}},
			httpDrainBudget:  40 * time.Millisecond,
			workerStopBudget: 40 * time.Millisecond,
		})
	}()
	client := &http.Client{Timeout: time.Second}
	go func() {
		resp, getErr := client.Get("http://" + listener.Addr().String())
		if getErr == nil {
			_ = resp.Body.Close()
		}
	}()
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("request did not reach handler")
	}
	started := time.Now()
	cancel()
	select {
	case runErr := <-done:
		if runErr == nil || !errors.Is(runErr, context.DeadlineExceeded) {
			t.Fatalf("shutdown error=%v", runErr)
		}
		if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
			t.Fatalf("shutdown exceeded bound: %s", elapsed)
		}
	case <-time.After(time.Second):
		t.Fatal("shutdown did not honor drain budget")
	}
}

func TestSystemdStopBudgetExceedsLifecycleBudgets(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/systemd/handoffkeep.service")
	if err != nil {
		t.Fatal(err)
	}
	var stopBudget time.Duration
	for _, line := range strings.Split(string(raw), "\n") {
		value, found := strings.CutPrefix(strings.TrimSpace(line), "TimeoutStopSec=")
		if !found {
			continue
		}
		seconds, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("TimeoutStopSec must be integer seconds: %q", value)
		}
		stopBudget = time.Duration(seconds) * time.Second
	}
	if stopBudget <= httpDrainBudget+workerStopBudget {
		t.Fatalf("TimeoutStopSec=%s must exceed HTTP+worker budgets=%s", stopBudget, httpDrainBudget+workerStopBudget)
	}
	if !strings.Contains(string(raw), "default KillSignal=SIGTERM") {
		t.Fatal("service does not document the SIGTERM contract")
	}
}
