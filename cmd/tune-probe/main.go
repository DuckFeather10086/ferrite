// Command tune-probe is a smoke test for proc.Spawn + tuner.DvbrCLI.
// It tunes the requested channel, dumps N bytes of raw TS to a file,
// reports throughput, and exits.
//
// Example:
//   tune-probe \
//     -dvbr ../dvbr/target/release/dvbr \
//     -channels ../isdbd/channels.json \
//     -adapter 0 -bytes 8M -out /tmp/probe.ts \
//     "TOKYO MX1"
//
// Output is encrypted TS — pipe through b25 to inspect with mediainfo.
// This command does NOT acquire any application-level lock; assume
// the adapter is free before running it.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/DuckFeather10086/isdbd/internal/tuner"
)

func main() {
	var (
		dvbrBin     = flag.String("dvbr", "dvbr", "path to dvbr binary")
		channelFile = flag.String("channels", "channels.json", "path to channels.json")
		adapter     = flag.Int("adapter", 0, "DVB adapter number")
		outPath     = flag.String("out", "/tmp/probe.ts", "output file for raw TS")
		bytesStr    = flag.String("bytes", "8M", "bytes to capture (supports K/M/G suffix)")
		timeoutSec  = flag.Int("timeout", 60, "overall timeout in seconds")
	)
	flag.Parse()

	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: tune-probe [flags] <channel>")
		flag.Usage()
		os.Exit(2)
	}
	channel := flag.Arg(0)

	target, err := parseSize(*bytesStr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bad -bytes:", err)
		os.Exit(2)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	})))

	ctx, cancel := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	ctx, cancelTimeout := context.WithTimeout(ctx, time.Duration(*timeoutSec)*time.Second)
	defer cancelTimeout()

	cli := &tuner.DvbrCLI{
		BinPath:      *dvbrBin,
		ChannelsFile: *channelFile,
	}

	slog.Info("spawning dvbr tune", "channel", channel, "adapter", *adapter, "target_bytes", target)
	stream, err := cli.Tune(ctx, *adapter, channel)
	if err != nil {
		slog.Error("tune failed", "err", err)
		os.Exit(1)
	}
	defer stream.Close()

	out, err := os.Create(*outPath)
	if err != nil {
		slog.Error("open output", "err", err)
		os.Exit(1)
	}
	defer out.Close()

	start := time.Now()
	written, err := io.CopyN(out, stream, target)
	elapsed := time.Since(start)

	if err != nil && err != io.EOF {
		slog.Error("read/write failed", "written", written, "elapsed_s", elapsed.Seconds(), "err", err)
		os.Exit(1)
	}

	mbps := float64(written) * 8 / 1e6 / elapsed.Seconds()
	slog.Info("done",
		"out", *outPath,
		"bytes", written,
		"elapsed_s", fmt.Sprintf("%.2f", elapsed.Seconds()),
		"mbps", fmt.Sprintf("%.2f", mbps),
	)
}

func parseSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty")
	}
	mult := int64(1)
	switch last := s[len(s)-1]; last {
	case 'K', 'k':
		mult, s = 1<<10, s[:len(s)-1]
	case 'M', 'm':
		mult, s = 1<<20, s[:len(s)-1]
	case 'G', 'g':
		mult, s = 1<<30, s[:len(s)-1]
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}
