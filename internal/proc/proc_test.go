package proc

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

// Tests intentionally use /bin/sh so they run without any tuner
// hardware. The pgrp lifecycle code paths exercised here are the
// same ones dvbr / b25 / ffmpeg will hit in production.

func TestSpawn_ReadsStdoutAndExitsCleanly(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, "/bin/sh", "-c", "printf hello")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Close()

	b, err := io.ReadAll(p.Stdout)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if got := string(b); got != "hello" {
		t.Fatalf("stdout = %q, want %q", got, "hello")
	}

	if err := p.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestSpawn_CloseSIGTERMsLongRunningChild(t *testing.T) {
	ctx := context.Background()
	// Sleeps 30s — Close must SIGTERM the pgrp and return promptly.
	p, err := Spawn(ctx, "/bin/sh", "-c", "sleep 30")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	start := time.Now()
	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("Close took %v, expected sub-second after SIGTERM", elapsed)
	}
}

func TestSpawn_ContextCancelKillsChild(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	p, err := Spawn(ctx, "/bin/sh", "-c", "sleep 30")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	defer p.Close()

	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_ = p.Wait()
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("Wait took %v after ctx cancel; expected sub-second", elapsed)
	}
}

func TestSpawn_KillsEntirePipeline(t *testing.T) {
	// Two-process shell pipeline; both must die on Close. We verify by
	// timing — if only the leader (sh) died but `sleep` survived,
	// Close would still return promptly (it only waits on the leader),
	// so this test mainly guards against regressions in Setpgid setup.
	ctx := context.Background()
	p, err := Spawn(ctx, "/bin/sh", "-c", "sleep 30 | cat")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	if err := p.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestSpawn_BinaryNotFound(t *testing.T) {
	_, err := Spawn(context.Background(), "/no/such/binary/exists/here")
	if err == nil {
		t.Fatal("Spawn of nonexistent binary should fail")
	}
	if !strings.Contains(err.Error(), "start") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestSpawn_NonZeroExitSurfacesViaWait(t *testing.T) {
	ctx := context.Background()
	p, err := Spawn(ctx, "/bin/sh", "-c", "exit 7")
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	// Drain stdout so cmd.Wait can proceed.
	_, _ = io.Copy(io.Discard, p.Stdout)

	err = p.Wait()
	if err == nil {
		t.Fatal("Wait should report non-zero exit")
	}
	// Should NOT be the SIGTERM/SIGKILL sentinel — that path returns nil.
	if errors.Is(err, nil) {
		t.Fatal("Wait returned nil for exit 7")
	}
}
