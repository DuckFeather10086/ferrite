// Command isdbd is the ISDB-T tuner / EPG / recording daemon.
//
// Skeleton only. See ../../internal/*/ for the packages that will own
// each responsibility.
package main

import (
	"flag"
	"log/slog"
	"os"
)

func main() {
	cfgPath := flag.String("config", "configs/isdbd.toml", "path to daemon config")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	slog.Info("isdbd starting", "config", *cfgPath)
	slog.Warn("not implemented yet")
}
