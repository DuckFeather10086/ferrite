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

// Every case here is a real record out of channels.json — the file mixes
// three provenances (legacy mojibake names, curated ASCII keys, scanned
// broadcast names) and the display rule has to read well for all of them.
func TestDisplayName(t *testing.T) {
	cases := []struct {
		what    string
		ch      Channel
		want    string
		because string
	}{{
		what:    "legacy mojibake name, scanned alias",
		ch:      Channel{Name: "NHKEFl1El5~", Aliases: []string{"NHKEテレ1東京"}},
		want:    "NHKEテレ1東京",
		because: "the alias is the only readable candidate",
	}, {
		what:    "curated ASCII key",
		ch:      Channel{Name: "asahi", Aliases: []string{"|ÆìÓD+F|", "テレビ朝日"}},
		want:    "テレビ朝日",
		because: "skip the mojibake alias, take the Japanese one",
	}, {
		what:    "first Japanese alias wins",
		ch:      Channel{Name: "NHK_G", Aliases: []string{"NHK総合", "NHK General", "NHK総合1東京"}},
		want:    "NHK総合",
		because: "file order, same as lookup",
	}, {
		what:    "good name, mojibake alias",
		ch:      Channel{Name: "J：COMテレビ", Aliases: []string{"J!'COM|ÆìÓ", "u23656_473142857"}},
		want:    "J：COMテレビ",
		because: "the record's own name reads fine; never prefer an alias over it",
	}, {
		what:    "disambiguating suffix is kept",
		ch:      Channel{Name: "テレビ朝日_2", Aliases: []string{"テレビ朝日"}},
		want:    "テレビ朝日_2",
		because: "four services share one service name; dropping _2 merges them on screen",
	}, {
		what:    "deliberately ASCII, no aliases",
		ch:      Channel{Name: "TBS1"},
		want:    "TBS1",
		because: "nothing to improve",
	}, {
		what:    "ideographic space is not an improvement",
		ch:      Channel{Name: "TOKYO MX1", Aliases: []string{"TOKYO　MX1"}},
		want:    "TOKYO MX1",
		because: "U+3000 must not count as Japanese, or the alias wins for no gain",
	}, {
		what:    "placeholder with nothing better",
		ch:      Channel{Name: "515.14MHz#23864"},
		want:    "515.14MHz#23864",
		because: "an honest placeholder beats inventing a name",
	}}

	for _, tc := range cases {
		if got := tc.ch.DisplayName(); got != tc.want {
			t.Errorf("%s: DisplayName() = %q, want %q (%s)", tc.what, got, tc.want, tc.because)
		}
	}

	if (*Channel)(nil).DisplayName() != "" {
		t.Error("nil receiver should be empty, not panic")
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
