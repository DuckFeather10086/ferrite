package config

import (
	"os"
	"path/filepath"
	"reflect"
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

func TestLoad_RequiresChannelsFilePath(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "isdbd.toml")
	mustWrite(t, tomlPath, `
http_port = 8010
storage_root = "`+dir+`/var"
`)
	_, err := Load(tomlPath)
	if err == nil || !strings.Contains(err.Error(), "channels_file") {
		t.Fatalf("expected channels_file error, got %v", err)
	}
}

// But a path to a file that does not exist yet must load. A fresh install
// has no channel list and produces one by scanning at first start; failing
// here would make the only way to get the file be to already have it.
func TestLoad_AllowsAChannelsFileThatIsNotThereYet(t *testing.T) {
	dir := t.TempDir()
	tomlPath := filepath.Join(dir, "isdbd.toml")
	mustWrite(t, tomlPath, `
http_port = 8010
storage_root = "`+dir+`/var"
channels_file = "`+dir+`/not-scanned-yet.json"
`)
	if _, err := Load(tomlPath); err != nil {
		t.Fatalf("a missing channels.json must not block startup: %v", err)
	}
}

// The bare `adapters = [0]` form has to keep meaning what it always did,
// and the `[[adapter]]` form has to be the only other way of saying it.
func TestAdapterList(t *testing.T) {
	dir := t.TempDir()
	channelsPath := filepath.Join(dir, "channels.json")
	mustWrite(t, channelsPath, `{"version":1,"channels":[]}`)
	base := "http_port = 8010\nstorage_root = \"" + dir + "/var\"\nchannels_file = \"" + channelsPath + "\"\n"

	load := func(t *testing.T, body string) (*Daemon, error) {
		t.Helper()
		p := filepath.Join(dir, strings.ReplaceAll(t.Name(), "/", "_")+".toml")
		mustWrite(t, p, base+body)
		return Load(p)
	}

	t.Run("legacy list is terrestrial", func(t *testing.T) {
		d, err := load(t, "adapters = [0, 2]\n")
		if err != nil {
			t.Fatal(err)
		}
		want := []Adapter{{N: 0, Systems: []string{"ISDBT"}}, {N: 2, Systems: []string{"ISDBT"}}}
		if got := d.AdapterList(); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("table form, case normalized", func(t *testing.T) {
		d, err := load(t, "[[adapter]]\nn = 0\nsystems = [\"isdbt\"]\n\n[[adapter]]\nn = 1\nsystems = [\"ISDBS\"]\n")
		if err != nil {
			t.Fatal(err)
		}
		want := []Adapter{{N: 0, Systems: []string{"ISDBT"}}, {N: 1, Systems: []string{"ISDBS"}}}
		if got := d.AdapterList(); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
		// Defaults() seeds Adapters with [0]; writing only [[adapter]] must
		// not leave that behind as a phantom third frontend.
		if len(d.Adapters) != 0 {
			t.Fatalf("legacy list should be cleared, got %v", d.Adapters)
		}
	})

	t.Run("table form without systems is terrestrial", func(t *testing.T) {
		d, err := load(t, "[[adapter]]\nn = 3\n")
		if err != nil {
			t.Fatal(err)
		}
		want := []Adapter{{N: 3, Systems: []string{"ISDBT"}}}
		if got := d.AdapterList(); !reflect.DeepEqual(got, want) {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})

	t.Run("both forms is a mistake worth failing on", func(t *testing.T) {
		_, err := load(t, "adapters = [0]\n\n[[adapter]]\nn = 1\nsystems = [\"ISDBS\"]\n")
		if err == nil || !strings.Contains(err.Error(), "not both") {
			t.Fatalf("expected an either/or error, got %v", err)
		}
	})
}

// The three ways hls_root resolves. The unset-variable case is the one that
// matters: `hls_root = "$RUNTIME_DIRECTORY/hls"` is the config this repo
// ships, and outside systemd os.ExpandEnv alone would silently turn it into
// the absolute path "/hls".
func TestLiveRoot(t *testing.T) {
	dir := t.TempDir()
	d := &Daemon{StorageRoot: filepath.Join(dir, "var")}

	got, err := d.LiveRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "var", "hls"); got != want {
		t.Errorf("unset hls_root: got %q, want %q", got, want)
	}

	runtimeDir := filepath.Join(dir, "run")
	t.Setenv("FERRITE_TEST_RUNTIME_DIR", runtimeDir)
	d.HLSRoot = "$FERRITE_TEST_RUNTIME_DIR/hls"
	got, err = d.LiveRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(runtimeDir, "hls"); got != want {
		t.Errorf("expanded hls_root: got %q, want %q", got, want)
	}
	if st, err := os.Stat(got); err != nil || !st.IsDir() {
		t.Errorf("LiveRoot should create the directory: %v", err)
	}

	d.HLSRoot = "$FERRITE_TEST_UNSET_RUNTIME_DIR/hls"
	got, err = d.LiveRoot()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "var", "hls"); got != want {
		t.Errorf("unset variable should fall back to storage_root: got %q, want %q", got, want)
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
