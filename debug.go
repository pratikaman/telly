package main

import (
	"fmt"
	"os"
	"sync"
	"time"
)

var (
	dbgOn   bool
	dbgMu   sync.Mutex
	dbgFile *os.File
)

// initDebug enables a timestamped log when TELLY_LOG names a file.
func initDebug() {
	path := os.Getenv("TELLY_LOG")
	if path == "" {
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	dbgFile, dbgOn = f, true
}

func dbg(format string, args ...any) {
	if !dbgOn {
		return
	}
	dbgMu.Lock()
	defer dbgMu.Unlock()
	fmt.Fprintf(dbgFile, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05.000")}, args...)...)
}
