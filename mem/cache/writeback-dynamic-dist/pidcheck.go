package writeback_dynamic_dist

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"sync"
)

var (
	pidCheckOnce   sync.Once
	pidCheckLogger *log.Logger
)

// pidCheckLog writes a PID-CHECK message to both the console (stderr) and a
// file named after the running benchmark (the executable name). The file is
// opened lazily on first use and appended to thereafter.
func pidCheckLog(format string, args ...interface{}) {
	pidCheckOnce.Do(func() {
		benchName := filepath.Base(os.Args[0])
		var w io.Writer = os.Stderr
		f, err := os.OpenFile(
			benchName+".pidcheck.log",
			os.O_CREATE|os.O_WRONLY|os.O_APPEND,
			0644,
		)
		if err == nil {
			w = io.MultiWriter(os.Stderr, f)
		}
		pidCheckLogger = log.New(w, "", log.LstdFlags)
	})
	pidCheckLogger.Printf(format, args...)
}
