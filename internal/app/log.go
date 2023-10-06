package app

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
)

// Trace writes tracing message to STDERR.
func Trace(msg string, args ...any) {
	if level <= levelTrace {
		write("trace", msg, args...)
	}
}

// Info writes informational message to STDERR.
func Info(msg string, args ...any) {
	if level <= levelInfo {
		write("info", msg, args...)
	}
}

// Warn writes warning message to STDERR.
func Warn(msg string, args ...any) {
	if level <= levelWarn {
		write("warn", msg, args...)
	}
}

// Error writes error message to STDERR and requests application shutdown.
func Error(msg string, args ...any) {
	writeErr(1, msg, args...)
}

func writeErr(ret int32, msg string, args ...any) {
	if code.CompareAndSwap(0, ret) {
		write("error", msg, args...)
		Shutdown()
	} else {
		Warn(msg, args...)
	}
}

// This function does the actual writing.
func write(kind, msg string, args ...any) {
	addr := pool.Get().(*[]byte)

	// header
	buff := append(append(*addr, kind...), ':', '\t')

	// message body
	switch {
	case len(msg) == 0:
		buff = append(buff, "<empty message>"...)
	case len(args) > 0:
		buff = fmt.Appendf(buff, msg, args...)
	default:
		buff = append(buff, msg...)
	}

	buff = append(bytes.TrimRight(buff, " \t\r\n"), '\n')

	flush(buff)

	*addr = buff[:0]
	pool.Put(addr)
}

func flush(buff []byte) {
	mu.Lock()
	defer mu.Unlock()

	if _, err := os.Stderr.Write(buff); err != nil {
		// There is nothing we can do here, except just exit, and as the last resort
		// we return code 125 (the max. portable error code, see https://pkg.go.dev/os#Exit)
		os.Exit(125)
	}
}

// SetLogLevel changes the logging level.
func SetLogLevel(logLevel string) (err error) {
	switch strings.ToLower(logLevel) {
	case "error":
		level = levelErr
	case "warning":
		level = levelWarn
	case "info":
		level = levelInfo
	case "trace":
		level = levelTrace
	default:
		err = errors.New("invalid logging level: " + strconv.Quote(logLevel))
	}

	return
}

// logging levels
const (
	levelTrace = iota
	levelInfo
	levelWarn
	levelErr
)

var (
	// pool of buffers
	pool = sync.Pool{
		New: func() any {
			buff := make([]byte, 0, 256)
			return &buff
		},
	}

	// mutex
	mu sync.Mutex

	// tracing level
	level = levelInfo
)
