package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "agent.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

func host() (string, error) { return "web-1", nil }

func TestLoadFillsDefaults(t *testing.T) {
	t.Setenv("TAILCOVE_TOKEN", "")

	cfg, err := Load(write(t, `{"token":"abc","logs":[{"path":"/var/log/app.log"}]}`), host)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Endpoint != DefaultEndpoint || cfg.Host != "web-1" || cfg.Metrics.IntervalSeconds != 30 || !cfg.MetricsEnabled() || cfg.StateFile != DefaultStateFile {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestEnvironmentOverridesTheFile(t *testing.T) {
	t.Setenv("TAILCOVE_TOKEN", "from-env")
	t.Setenv("TAILCOVE_HOST", "db-2")
	t.Setenv("TAILCOVE_LOG_PATHS", "/a.log, /b/*.log")
	t.Setenv("TAILCOVE_METRICS", "false")

	cfg, err := Load(write(t, `{"token":"abc","host":"x","logs":[{"path":"/file.log"}]}`), host)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Token != "from-env" || cfg.Host != "db-2" || len(cfg.Logs) != 2 || cfg.Logs[1].Path != "/b/*.log" || cfg.MetricsEnabled() {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestFlagsThatSwitchThingsOffAreParsedStrictly(t *testing.T) {
	for _, bad := range []string{"no", "off", "disabled", "ture", "2", "yes"} {
		t.Setenv("TAILCOVE_TOKEN", "abc")
		t.Setenv("TAILCOVE_METRICS", bad)

		if _, err := Load("", host); err == nil || !strings.Contains(err.Error(), "TAILCOVE_METRICS") {
			t.Errorf("TAILCOVE_METRICS=%q should be refused, got %v", bad, err)
		}
	}

	for value, want := range map[string]bool{"true": true, "TRUE": true, "1": true, "false": false, "0": false} {
		got, err := ParseBool(value)
		if err != nil || got != want {
			t.Errorf("ParseBool(%q) = %v, %v", value, got, err)
		}
	}
}

func TestValidation(t *testing.T) {
	t.Setenv("TAILCOVE_TOKEN", "")
	t.Setenv("TAILCOVE_METRICS", "")

	cases := map[string]string{
		"no token":                  `{"logs":[{"path":"/a"}]}`,
		"plain http to a real host": `{"token":"t","endpoint":"http://ingest.example.com","logs":[{"path":"/a"}]}`,
		"not a url":                 `{"token":"t","endpoint":"ingest.example.com","logs":[{"path":"/a"}]}`,
		"interval too short":        `{"token":"t","metrics":{"interval_seconds":5},"logs":[{"path":"/a"}]}`,
		"empty path":                `{"token":"t","logs":[{"path":" "}]}`,
		"nothing to do":             `{"token":"t","metrics":{"enabled":false}}`,
		"unknown field":             `{"token":"t","tokn":"typo","logs":[{"path":"/a"}]}`,
	}

	for name, body := range cases {
		if _, err := Load(write(t, body), host); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestPlainHTTPIsAllowedOnLoopbackAndWhenAccepted(t *testing.T) {
	t.Setenv("TAILCOVE_TOKEN", "")

	for _, body := range []string{
		`{"token":"t","endpoint":"http://localhost:8080","logs":[{"path":"/a"}]}`,
		`{"token":"t","endpoint":"http://ingest.internal","insecure":true,"logs":[{"path":"/a"}]}`,
	} {
		if _, err := Load(write(t, body), host); err != nil {
			t.Errorf("%s: %v", body, err)
		}
	}
}

func TestLoadProbeNeedsAProbeToken(t *testing.T) {
	t.Setenv("TAILCOVE_TOKEN", "")

	if _, err := LoadProbe(write(t, `{"token":"abc"}`), host); err == nil || !strings.Contains(err.Error(), "tcp_") {
		t.Fatalf("a source token must not start a probe, got %v", err)
	}

	cfg, err := LoadProbe(write(t, `{"token":"tcp_abc"}`), host)
	if err != nil {
		t.Fatal(err)
	}

	if cfg.Endpoint != DefaultEndpoint {
		t.Fatalf("endpoint = %q", cfg.Endpoint)
	}
}

func TestLoadProbeNeedsNoLogsOrMetrics(t *testing.T) {
	t.Setenv("TAILCOVE_TOKEN", "")
	t.Setenv("TAILCOVE_METRICS", "false")

	if _, err := LoadProbe(write(t, `{"token":"tcp_abc"}`), host); err != nil {
		t.Fatalf("probe mode has nothing to follow: %v", err)
	}
}

func TestLoadProbeRefusesAPlainHTTPEndpoint(t *testing.T) {
	t.Setenv("TAILCOVE_TOKEN", "")

	if _, err := LoadProbe(write(t, `{"token":"tcp_abc","endpoint":"http://tailcove.example"}`), host); err == nil {
		t.Fatal("plain http to a remote host must be refused")
	}
}
