package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCheckBinariesFindsWhatIsMissing(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "dvb-rs")
	if err := os.WriteFile(good, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	notExec := filepath.Join(dir, "b25-rs")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	d := &Daemon{
		DvbrBin:   good,
		B25Bin:    notExec,
		FFmpegBin: filepath.Join(dir, "gone"),
		// Empty is a configuration, not a fault: no decoder means no
		// captions, which is how a box with no arib-caption is meant to run.
		AribCaptionBin: "",
		FFprobeBin:     "",
	}
	pf := d.CheckBinaries()

	if pf.OK() {
		t.Fatal("reported healthy with two broken paths")
	}
	got := map[string]Problem{}
	for _, p := range pf.Problems {
		got[p.Setting] = p
	}
	if len(got) != 2 {
		t.Fatalf("problems = %v, want b25_bin and ffmpeg_bin only", pf.Problems)
	}
	if p, ok := got["b25_bin"]; !ok || p.Err != "not executable" {
		t.Errorf("b25_bin: %+v", p)
	}
	if p, ok := got["ffmpeg_bin"]; !ok || p.Err != "no such file" {
		t.Errorf("ffmpeg_bin: %+v", p)
	}
	// ffmpeg is one of the two the daemon cannot work without, so this has
	// to read as fatal — that is what separates "no subtitles tonight" from
	// "this box is not a television".
	if !pf.Fatal() {
		t.Error("a missing ffmpeg_bin did not report as fatal")
	}
}

// A bare name is looked up on $PATH, the way exec.Command would — so the
// default config, which names `ffmpeg` and not a path, is checked correctly.
func TestCheckBinariesLooksBareNamesUpOnPath(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ffmpeg"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	d := &Daemon{DvbrBin: "ffmpeg", FFmpegBin: "definitely-not-installed"}
	pf := d.CheckBinaries()

	if len(pf.Problems) != 1 || pf.Problems[0].Setting != "ffmpeg_bin" {
		t.Fatalf("problems = %v, want ffmpeg_bin alone", pf.Problems)
	}
	if pf.Problems[0].Err != "not on $PATH" {
		t.Errorf("err = %q", pf.Problems[0].Err)
	}
}

// A healthy box says nothing, and /api/status omits the field entirely.
func TestCheckBinariesQuietWhenEverythingIsThere(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "x")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := &Daemon{DvbrBin: bin, B25Bin: bin, FFmpegBin: bin, FFprobeBin: bin, AribCaptionBin: bin}
	pf := d.CheckBinaries()
	if !pf.OK() || pf.Fatal() || pf.Summary() != "" {
		t.Fatalf("%+v", pf)
	}
}
