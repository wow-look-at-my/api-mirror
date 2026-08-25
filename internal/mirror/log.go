package mirror

import (
	"fmt"
	"log/slog"
	"os"
)

// logger is where the engine reports. It is a package var so a test can capture
var logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// logf reports something that went wrong on a path that continues anyway. A
func logf(format string, args ...any) {
	logger.Error(fmt.Sprintf(format, args...))
}
