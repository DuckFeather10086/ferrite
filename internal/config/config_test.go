package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadChannels_ParsesAliasesAndTuning(t *testing.T) {
	path := filepath.Join("testdata", "channels.json")
	mustWrite(t, path, `{
		"version": 1,
		"channels": [
			{
				"name": "TOKYO MX1",
				"aliases": ["mx"],
				"legacy_zap_section": "TOKYO MX1",
				"tuning": {"SERVICE_ID": "23608", "FREQUENCY": "527142857"}
			}
		]
	}`)
	defer os.RemoveAll("testdata")

	c, err := LoadChannels(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Channels) != 1 {
		t.Fatalf("got %d channels", len(c.Channels))
	}

	ch := c.Find("mx")
	if ch == nil {
		t.Fatal("alias lookup failed")
	}
	if ch.Name != "TOKYO MX1" {
		t.Fatalf("wrong channel: %q", ch.Name)
	}
	if ch.ServiceID() != 23608 {
		t.Fatalf("ServiceID: got %d", ch.ServiceID())
	}

	if c.Find("does-not-exist") != nil {
		t.Fatal("expected nil for missing channel")
	}
}

func TestLoad_DefaultsAndValidate(t *testing.T) {
	dir := t.TempDir()
	channelsPath := filepath.Join(dir, "channels.json")
	mustWrite(t, channelsPath, `{"version":1,"channels":[]}`)

	tomlPath := filepath.Join(dir, "isdbd.toml")
	mustWrite(t, tomlPath, `
http_port = 9000
storage_root = "`+dir+`/var"
adapters = [0, 1]
channels_file = "`+channelsPath+`"
`)

	d, err := Load(tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if d.HTTPPort != 9000 {
		t.Fatalf("HTTPPort: %d", d.HTTPPort)
	}
	if d.DvbrBin != "dvb-rs" { // default
		t.Fatalf("DvbrBin default lost: %q", d.DvbrBin)
	}
	if len(d.Adapters) != 2 {
		t.Fatalf("Adapters: %v", d.Adapters)
	}
}

func TestLoad_RejectsBadPort(t *testing.T) {
	dir := t.TempDir()
	channelsPath := filepath.Join(dir, "channels.json")
	mustWrite(t, channelsPath, `{"version":1,"channels":[]}`)

	tomlPath := filepath.Join(dir, "isdbd.toml")
	mustWrite(t, tomlPath, `
http_port = 0
storage_root = "`+dir+`/var"
channels_file = "`+channelsPath+`"
`)
	_, err := Load(tomlPath)
	if err == nil || !strings.Contains(err.Error(), "http_port") {
		t.Fatalf("expected http_port error, got %v", err)
	}
}

func TestLoad_RejectsMissingChannelsFile(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "isdbd.toml")
	mustWrite(t, tomlPath, `
http_port = 8010
storage_root = "`+dir+`/var"
channels_file = "/does/not/exist.json"
`)
	_, err := Load(tomlPath)
	if err == nil || !strings.Contains(err.Error(), "channels_file") {
		t.Fatalf("expected channels_file error, got %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
