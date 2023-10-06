// Package app provides a minimalistic framework for Go applications. It is designed
// mostly for command line programs.
//
// Main features:
//   - Goroutines that are waited upon before application exit;
//   - Application lifetime control via main context;
//   - Formatted logging to STDERR;
package app

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
)

// Shutdown terminates the main application context.
var ctx, Shutdown = context.WithCancel(context.Background())

// Context returns the main application context.
func Context() context.Context { return ctx }

// Shut returns "done" channel of the the main context.
func Shut() <-chan struct{} { return ctx.Done() }

// Running checks if application shutdown is not requested yet.
func Running() bool { return ctx.Err() == nil }

// Failed checks if the application is shutting down with a non-zero exit code.
func Failed() bool { return code.Load() != 0 }

// Go invokes the given function in a separate goroutine registered with the runtime,
// so that the application will wait for the function to complete before exiting.
// Any non-nil error from the function is reported via app.Error(), causing application shutdown.
func Go(fn func() error) {
	wg.Add(1)

	go func() {
		defer wg.Done()

		if err := fn(); err != nil {
			Error(err.Error())
		}
	}()
}

// Run invokes the main application function; should be called exactly once upon startup.
// Application terminates when the application function exits. Function app.Run itself never returns.
func Run(fn func() error) {
	// attach signal handlers
	sigch := make(chan os.Signal, 4)

	signal.Notify(sigch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)

	// start signal monitor
	go func() {
		for { // yes, forever
			sig := <-sigch

			if sigNo, ok := sig.(syscall.Signal); ok {
				writeErr(2, "signal %d: %s", sigNo, sig)
			} else {
				writeErr(2, "signal: %s", sig)
			}
		}
	}()

	// invoke the main application function
	if err := fn(); err != nil {
		Error(err.Error())
	} else {
		Shutdown()
	}

	// wait for all goroutines to terminate
	wg.Wait()

	// invoke AtExit handlers
	for i := len(exitHandlers) - 1; i >= 0; i-- {
		exitHandlers[i](Failed())
	}

	// exit
	os.Exit(int(code.Load()))
}

// AtExit registers the given function for being called upon application exit.
// All registered functions will be invoked in LIFO order, and each function
// will be passed a flag indicating application failure.
func AtExit(fn func(bool)) {
	exitMutex.Lock()
	defer exitMutex.Unlock()

	exitHandlers = append(exitHandlers, fn)
}

var (
	wg           sync.WaitGroup // wait group for all registered goroutines.
	code         atomic.Int32   // application return code
	exitMutex    sync.Mutex     // mutex for AtExit
	exitHandlers []func(bool)   // stack of functions to call upon exit
)
