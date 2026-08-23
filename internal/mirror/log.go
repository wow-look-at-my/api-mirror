package mirror

import (
	"fmt"
	"log/slog"
	"os"
)

// logger is where the engine reports. It is a package var so a test can capture
// what a degraded path said, which is the only way to assert that a fallback
// announced itself instead of failing quietly.
var logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// logf reports something that went wrong on a path that continues anyway. A
// degraded path is louder than the happy path, never quieter: the whole cost of
// a silent fallback is that nobody learns the answer got worse.
func logf(format string, args ...any) {
	logger.Error(fmt.Sprintf(format, args...))
}
