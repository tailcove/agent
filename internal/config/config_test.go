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

func TestKubernetesDefaults(t *testing.T) {
	t.Setenv("TAILCOVE_TOKEN", "abc")
	t.Setenv("NODE_NAME", "node-7")

	cfg, err := Load(write(t, `{"kubernetes":{"enabled":true}}`), host)
	if err != nil {
		t.Fatal(err)
	}

	k := cfg.Kubernetes
	if k.NodeName != "node-7" || k.PodsDir != DefaultPodsDir || k.Host != "pod" || !k.APIEnabled() || !k.MetricsEnabled() || len(k.Labels) != len(DefaultKubernetesLabels) {
		t.Fatalf("unexpected defaults: %+v", k)
	}
}

func TestKubernetesCanBeTheOnlyThingConfigured(t *testing.T) {
	t.Setenv("TAILCOVE_TOKEN", "abc")
	t.Setenv("TAILCOVE_METRICS", "false")
	t.Setenv("TAILCOVE_KUBERNETES", "true")
	t.Setenv("NODE_NAME", "n")

	if _, err := Load("", host); err != nil {
		t.Fatal(err)
	}
}

func TestKubernetesSettingsAreValidated(t *testing.T) {
	cases := map[string]string{
		`{"kubernetes":{"enabled":true,"host":"cluster"}}`:                             "host",
		`{"kubernetes":{"enabled":true,"labels":["bad key"]}}`:                         "label key",
		`{"kubernetes":{"enabled":true,"routes":[{"token":"t"}]}}`:                     "matches everything",
		`{"kubernetes":{"enabled":true,"routes":[{"namespace":"a","token":"t t"}]}}`:   "token",
		`{"kubernetes":{"enabled":true,"metrics":{"interval_seconds":1}}}`:             "interval_seconds",
		`{"kubernetes":{"enabled":true,"multiline":{"mode":"pattern"}}}`:               "pattern",
		`{"kubernetes":{"enabled":true,"multiline":{"mode":"pattern","pattern":"("}}}`: "multiline pattern",
		`{"kubernetes":{"enabled":true,"multiline":{"mode":"sometimes"}}}`:             "multiline mode",
		`{"kubernetes":{"enabled":true,"metrics":{"max_series":-1}}}`:                  "max_series",
	}

	for body, want := range cases {
		t.Setenv("TAILCOVE_TOKEN", "abc")
		t.Setenv("NODE_NAME", "n")

		if _, err := Load(write(t, body), host); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: want error containing %q, got %v", body, want, err)
		}
	}
}

func TestKubernetesFlagsAreParsedStrictly(t *testing.T) {
	t.Setenv("TAILCOVE_TOKEN", "abc")
	t.Setenv("NODE_NAME", "n")
	t.Setenv("TAILCOVE_KUBERNETES", "yes")

	if _, err := Load("", host); err == nil || !strings.Contains(err.Error(), "TAILCOVE_KUBERNETES") {
		t.Fatalf("got %v", err)
	}
}

func TestKubernetesEnvironmentLists(t *testing.T) {
	t.Setenv("TAILCOVE_TOKEN", "abc")
	t.Setenv("NODE_NAME", "n")
	t.Setenv("TAILCOVE_KUBERNETES", "true")
	t.Setenv("TAILCOVE_K8S_EXCLUDE_NAMESPACES", "kube-system, monitoring")
	t.Setenv("TAILCOVE_K8S_HOST", "label:app")

	cfg, err := Load("", host)
	if err != nil {
		t.Fatal(err)
	}

	if len(cfg.Kubernetes.ExcludeNamespaces) != 2 || cfg.Kubernetes.ExcludeNamespaces[1] != "monitoring" || cfg.Kubernetes.Host != "label:app" {
		t.Fatalf("unexpected: %+v", cfg.Kubernetes)
	}
}
