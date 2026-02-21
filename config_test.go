package overseer

import (
	"os"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// TestLoadConfig_Defaults verifies that an empty path returns a Config with
// Listen=":8080" and DB="./overseer.db" without error.
func TestLoadConfig_Defaults(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatalf("LoadConfig(\"\") returned error: %v", err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q; want %q", cfg.Listen, ":8080")
	}
	if cfg.DB != "./overseer.db" {
		t.Errorf("DB = %q; want %q", cfg.DB, "./overseer.db")
	}
}

// TestLoadConfig_YAML verifies that a valid YAML config file is parsed correctly.
func TestLoadConfig_YAML(t *testing.T) {
	const yamlContent = `
listen: ":9000"
db: "./test.db"
log_file: "/var/log/overseer.jsonl"
trusted_cidrs:
  - "127.0.0.1/32"
  - "10.0.0.0/8"
retry:
  restart_delay: 5s
  error_window: 1m
  error_threshold: 3
task_pool:
  limit: 10
actions:
  stream:
    meta:
      description: "Start streaming task"
    type: exec
    retry:
      restart_delay: 10s
    config:
      entrypoint: /opt/bin/stream
`
	f, err := os.CreateTemp("", "overseer-config-*.yaml")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(f.Name())

	if _, err := f.WriteString(yamlContent); err != nil {
		t.Fatalf("failed to write temp file: %v", err)
	}
	f.Close()

	cfg, err := LoadConfig(f.Name())
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}

	if cfg.Listen != ":9000" {
		t.Errorf("Listen = %q; want %q", cfg.Listen, ":9000")
	}
	if cfg.DB != "./test.db" {
		t.Errorf("DB = %q; want %q", cfg.DB, "./test.db")
	}
	if cfg.LogFile != "/var/log/overseer.jsonl" {
		t.Errorf("LogFile = %q; want %q", cfg.LogFile, "/var/log/overseer.jsonl")
	}
	if len(cfg.TrustedCIDRs) != 2 {
		t.Errorf("TrustedCIDRs len = %d; want 2", len(cfg.TrustedCIDRs))
	}
	if cfg.Retry.RestartDelay != 5*time.Second {
		t.Errorf("Retry.RestartDelay = %v; want 5s", cfg.Retry.RestartDelay)
	}
	if cfg.Retry.ErrorWindow != time.Minute {
		t.Errorf("Retry.ErrorWindow = %v; want 1m", cfg.Retry.ErrorWindow)
	}
	if cfg.Retry.ErrorThreshold != 3 {
		t.Errorf("Retry.ErrorThreshold = %d; want 3", cfg.Retry.ErrorThreshold)
	}
	if cfg.TaskPool.Limit != 10 {
		t.Errorf("TaskPool.Limit = %d; want 10", cfg.TaskPool.Limit)
	}
	stream, ok := cfg.Actions["stream"]
	if !ok {
		t.Fatal("Actions[\"stream\"] not found")
	}
	if stream.Meta.Description != "Start streaming task" {
		t.Errorf("Actions[stream].Meta.Description = %q; want %q", stream.Meta.Description, "Start streaming task")
	}
	if stream.Type != "exec" {
		t.Errorf("Actions[stream].Type = %q; want %q", stream.Type, "exec")
	}
	if stream.Retry.RestartDelay != 10*time.Second {
		t.Errorf("Actions[stream].Retry.RestartDelay = %v; want 10s", stream.Retry.RestartDelay)
	}
	entrypoint, _ := stream.Config["entrypoint"].(string)
	if entrypoint != "/opt/bin/stream" {
		t.Errorf("Actions[stream].Config[entrypoint] = %q; want %q", entrypoint, "/opt/bin/stream")
	}
}

// TestLoadConfig_FileNotFound verifies that a non-existent path returns defaults rather than an error.
func TestLoadConfig_FileNotFound(t *testing.T) {
	cfg, err := LoadConfig("/nonexistent/path/to/config.yaml")
	if err != nil {
		t.Fatalf("LoadConfig with missing file returned error: %v", err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("Listen = %q; want %q", cfg.Listen, ":8080")
	}
	if cfg.DB != "./overseer.db" {
		t.Errorf("DB = %q; want %q", cfg.DB, "./overseer.db")
	}
}

// TestLoadConfig_InvalidYAML verifies that malformed YAML returns an error.
func TestLoadConfig_InvalidYAML(t *testing.T) {
	f, err := os.CreateTemp("", "overseer-bad-*.yaml")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(f.Name())
	f.WriteString(": bad: yaml: {{{{")
	f.Close()

	_, err = LoadConfig(f.Name())
	if err == nil {
		t.Error("expected error for invalid YAML, got nil")
	}
}

// TestParamSpec_YAML_Null verifies that a null YAML value produces a required
// parameter (Default=nil, Validate="").
func TestParamSpec_YAML_Null(t *testing.T) {
	const src = `param: ~`
	var out struct {
		Param ParamSpec `yaml:"param"`
	}
	if err := yaml.Unmarshal([]byte(src), &out); err != nil {
		t.Fatalf("yaml.Unmarshal error: %v", err)
	}
	if out.Param.Default != nil {
		t.Errorf("Default = %v; want nil (required param)", out.Param.Default)
	}
	if out.Param.Validate != "" {
		t.Errorf("Validate = %q; want \"\"", out.Param.Validate)
	}
}

// TestParamSpec_YAML_String verifies that a plain string YAML value produces an
// optional parameter with the string as its default.
func TestParamSpec_YAML_String(t *testing.T) {
	const src = `param: "chaturbate"`
	var out struct {
		Param ParamSpec `yaml:"param"`
	}
	if err := yaml.Unmarshal([]byte(src), &out); err != nil {
		t.Fatalf("yaml.Unmarshal error: %v", err)
	}
	if out.Param.Default == nil {
		t.Fatal("Default is nil; want pointer to \"chaturbate\"")
	}
	if *out.Param.Default != "chaturbate" {
		t.Errorf("Default = %q; want %q", *out.Param.Default, "chaturbate")
	}
	if out.Param.Validate != "" {
		t.Errorf("Validate = %q; want \"\"", out.Param.Validate)
	}
}

// TestParamSpec_YAML_Map verifies that a YAML map with default and validate keys
// produces a full ParamSpec.
func TestParamSpec_YAML_Map(t *testing.T) {
	const src = `
param:
  default: "mydefault"
  validate: "size(value) > 0"
`
	var out struct {
		Param ParamSpec `yaml:"param"`
	}
	if err := yaml.Unmarshal([]byte(src), &out); err != nil {
		t.Fatalf("yaml.Unmarshal error: %v", err)
	}
	if out.Param.Default == nil {
		t.Fatal("Default is nil; want pointer to \"mydefault\"")
	}
	if *out.Param.Default != "mydefault" {
		t.Errorf("Default = %q; want %q", *out.Param.Default, "mydefault")
	}
	if out.Param.Validate != "size(value) > 0" {
		t.Errorf("Validate = %q; want %q", out.Param.Validate, "size(value) > 0")
	}
}

// TestParamSpec_YAML_MapNoDefault verifies that a map with only validate and no
// default produces a required param with validation.
func TestParamSpec_YAML_MapNoDefault(t *testing.T) {
	const src = `
param:
  validate: "size(value) > 0"
`
	var out struct {
		Param ParamSpec `yaml:"param"`
	}
	if err := yaml.Unmarshal([]byte(src), &out); err != nil {
		t.Fatalf("yaml.Unmarshal error: %v", err)
	}
	if out.Param.Default != nil {
		t.Errorf("Default = %v; want nil", out.Param.Default)
	}
	if out.Param.Validate != "size(value) > 0" {
		t.Errorf("Validate = %q; want %q", out.Param.Validate, "size(value) > 0")
	}
}

// TestRetryPolicy_YAML verifies that duration strings are parsed correctly.
func TestRetryPolicy_YAML(t *testing.T) {
	const src = `
restart_delay: 5s
error_window: 1m30s
error_threshold: 5
`
	var p RetryPolicy
	if err := yaml.Unmarshal([]byte(src), &p); err != nil {
		t.Fatalf("yaml.Unmarshal error: %v", err)
	}
	if p.RestartDelay != 5*time.Second {
		t.Errorf("RestartDelay = %v; want 5s", p.RestartDelay)
	}
	want := 90 * time.Second
	if p.ErrorWindow != want {
		t.Errorf("ErrorWindow = %v; want %v", p.ErrorWindow, want)
	}
	if p.ErrorThreshold != 5 {
		t.Errorf("ErrorThreshold = %d; want 5", p.ErrorThreshold)
	}
}

// TestDisplacePolicy_Values verifies the DisplacePolicy string constants.
func TestDisplacePolicy_Values(t *testing.T) {
	tests := []struct {
		policy DisplacePolicy
		want   string
	}{
		{DisplaceFalse, "false"},
		{DisplaceTrue, "true"},
		{DisplaceProhibit, "prohibit"},
	}
	for _, tt := range tests {
		if string(tt.policy) != tt.want {
			t.Errorf("DisplacePolicy %v = %q; want %q", tt.policy, string(tt.policy), tt.want)
		}
	}
}

// TestLoadConfig_DefaultsAppliedAfterYAML verifies that defaults are only applied
// to fields not set by the YAML file (partial config scenario).
func TestLoadConfig_DefaultsAppliedAfterYAML(t *testing.T) {
	// Only set listen; db should get default.
	const yamlContent = `listen: ":7777"`
	f, err := os.CreateTemp("", "overseer-partial-*.yaml")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(f.Name())
	f.WriteString(yamlContent)
	f.Close()

	cfg, err := LoadConfig(f.Name())
	if err != nil {
		t.Fatalf("LoadConfig returned error: %v", err)
	}
	if cfg.Listen != ":7777" {
		t.Errorf("Listen = %q; want %q", cfg.Listen, ":7777")
	}
	if cfg.DB != "./overseer.db" {
		t.Errorf("DB = %q; want default %q", cfg.DB, "./overseer.db")
	}
}

// TestActionConfig_YAML verifies ActionConfig fields parse correctly.
func TestActionConfig_YAML(t *testing.T) {
	const src = `
actions:
  record:
    meta:
      description: "Record a stream"
    type: exec
    retry:
      restart_delay: 30s
      error_threshold: 2
    task_pool:
      limit: 5
    config:
      binary: /usr/bin/record
      flag: "--hls"
`
	var cfg Config
	if err := yaml.Unmarshal([]byte(src), &cfg); err != nil {
		t.Fatalf("yaml.Unmarshal error: %v", err)
	}
	ac, ok := cfg.Actions["record"]
	if !ok {
		t.Fatal("Actions[\"record\"] not found")
	}
	if ac.Meta.Description != "Record a stream" {
		t.Errorf("Meta.Description = %q; want %q", ac.Meta.Description, "Record a stream")
	}
	if ac.Type != "exec" {
		t.Errorf("Type = %q; want %q", ac.Type, "exec")
	}
	if ac.Retry.RestartDelay != 30*time.Second {
		t.Errorf("Retry.RestartDelay = %v; want 30s", ac.Retry.RestartDelay)
	}
	if ac.Retry.ErrorThreshold != 2 {
		t.Errorf("Retry.ErrorThreshold = %d; want 2", ac.Retry.ErrorThreshold)
	}
	if ac.TaskPool.Limit != 5 {
		t.Errorf("TaskPool.Limit = %d; want 5", ac.TaskPool.Limit)
	}
	if binary, _ := ac.Config["binary"].(string); binary != "/usr/bin/record" {
		t.Errorf("Config[binary] = %q; want %q", binary, "/usr/bin/record")
	}
}

