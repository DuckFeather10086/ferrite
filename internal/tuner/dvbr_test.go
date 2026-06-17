package tuner

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeScript drops an executable /bin/sh script into dir and returns
// its path. Used to stand in for the real dvbr / b25 binaries so the
// pipeline wiring can be exercised without a tuner or B-CAS card.
func writeScript(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestTune_DescramblesThroughB25 verifies that when B25Bin is set, the
// dvbr output is piped through b25 and the caller reads b25's (here:
// uppercased) output, not dvbr's raw bytes.
func TestTune_DescramblesThroughB25(t *testing.T) {
	dir := t.TempDir()
	// Fake dvbr: ignore all the tune args, emit a fixed payload, exit.
	dvbr := writeScript(t, dir, "dvbr", `printf 'scrambled-ts'`)
	// Fake b25: ignore "-v 0 - -", uppercase stdin → stdout.
	b25 := writeScript(t, dir, "b25", `exec tr 'a-z' 'A-Z'`)

	cli := &DvbrCLI{BinPath: dvbr, B25Bin: b25, ChannelsFile: "channels.json"}
	stream, err := cli.Tune(context.Background(), 0, "mx")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "SCRAMBLED-TS" {
		t.Fatalf("want descrambled %q, got %q", "SCRAMBLED-TS", got)
	}
}

// TestTune_RawWhenNoB25 verifies that with B25Bin empty the caller
// receives dvbr's stdout unmodified (free-to-air / no-card path).
func TestTune_RawWhenNoB25(t *testing.T) {
	dir := t.TempDir()
	dvbr := writeScript(t, dir, "dvbr", `printf 'raw-ts-bytes'`)

	cli := &DvbrCLI{BinPath: dvbr, ChannelsFile: "channels.json"}
	stream, err := cli.Tune(context.Background(), 0, "mx")
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	got, err := io.ReadAll(stream)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "raw-ts-bytes" {
		t.Fatalf("want raw %q, got %q", "raw-ts-bytes", got)
	}
}

// TestTune_CloseTearsDownPipeline verifies that closing a live stream
// reaps both subprocesses (including the dvbr source's child sleeps,
// via process-group kill) without hanging.
func TestTune_CloseTearsDownPipeline(t *testing.T) {
	dir := t.TempDir()
	// Fake dvbr: stream forever until killed.
	dvbr := writeScript(t, dir, "dvbr", `while true; do printf 'x'; sleep 0.05; done`)
	b25 := writeScript(t, dir, "b25", `exec cat`)

	cli := &DvbrCLI{BinPath: dvbr, B25Bin: b25, ChannelsFile: "channels.json"}
	stream, err := cli.Tune(context.Background(), 0, "mx")
	if err != nil {
		t.Fatal(err)
	}

	// Pull at least one byte so we know the pipeline is flowing.
	buf := make([]byte, 1)
	if _, err := io.ReadFull(stream, buf); err != nil {
		t.Fatalf("read first byte: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- stream.Close() }()
	select {
	case <-done:
		// Closed cleanly (SIGTERM, or SIGKILL after the 2s grace).
	case <-time.After(6 * time.Second):
		t.Fatal("stream.Close did not return; pipeline teardown hung")
	}

	// After teardown the stream must not block on further reads.
	if _, err := stream.Read(buf); err == nil {
		t.Fatal("expected read error after Close")
	}
}
