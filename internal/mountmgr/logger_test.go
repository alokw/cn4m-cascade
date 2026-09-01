package mountmgr

import (
	"io"
	"log/slog"
)

// testLogger discards output: these tests assert on behaviour, not logs.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}
