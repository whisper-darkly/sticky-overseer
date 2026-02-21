package main

import (
	"os"

	"gopkg.in/yaml.v3"
)

// DisplacePolicy controls whether a queue allows displacement of its items.
type DisplacePolicy string

const (
	DisplaceFalse    DisplacePolicy = "false"    // do not allow displacement
	DisplaceTrue     DisplacePolicy = "true"     // allow displacement
	DisplaceProhibit DisplacePolicy = "prohibit" // explicitly prohibit displacement
)

// Config is the top-level YAML configuration structure.
type Config struct {
	Listen       string                   `yaml:"listen"`
	DB           string                   `yaml:"db"`
	LogFile      string                   `yaml:"log_file"`
	TrustedCIDRs []string                 `yaml:"trusted_cidrs"`
	Retry        RetryPolicy              `yaml:"retry"`
	TaskPool     PoolConfig               `yaml:"task_pool"`
	Actions      map[string]ActionConfig  `yaml:"actions"`
}

// ActionConfig configures a named action handler.
type ActionConfig struct {
	Meta      ActionMeta     `yaml:"meta"`
	Type      string         `yaml:"type"`
	Retry     RetryPolicy    `yaml:"retry"`
	TaskPool  PoolConfig     `yaml:"task_pool"`
	Config    map[string]any `yaml:"config"`
	DedupeKey []string       `yaml:"dedupe_key"` // param names that form unique key; nil/empty = no dedup
}

// ActionMeta holds human-readable metadata about an action.
type ActionMeta struct {
	Description string `yaml:"description"`
}

// ParamSpec describes a single action parameter: optionally defaulted and/or CEL-validated.
type ParamSpec struct {
	Default  *string `json:"default,omitempty" yaml:"-"`  // nil = required parameter
	Validate string  `json:"validate,omitempty" yaml:"-"` // CEL expression; empty = no validation
}

// UnmarshalYAML handles three YAML forms for ParamSpec:
//   - null       → required parameter (Default=nil)
//   - "string"   → optional with given default value
//   - {default: "x", validate: "expr"} → full spec
func (p *ParamSpec) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		if value.Tag == "!!null" {
			// null → required parameter
			p.Default = nil
			p.Validate = ""
			return nil
		}
		// plain string → optional with default
		s := value.Value
		p.Default = &s
		p.Validate = ""
		return nil
	case yaml.MappingNode:
		type raw struct {
			Default  *string `yaml:"default"`
			Validate string  `yaml:"validate"`
		}
		var v raw
		if err := value.Decode(&v); err != nil {
			return err
		}
		p.Default = v.Default
		p.Validate = v.Validate
		return nil
	}
	return nil
}

// loadConfig reads the YAML config at path. If path is empty or the file does
// not exist, a default config is returned with sensible defaults applied.
func loadConfig(path string) (*Config, error) {
	cfg := &Config{}

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				// File not found — use defaults.
				applyConfigDefaults(cfg)
				return cfg, nil
			}
			return nil, err
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, err
		}
	}

	applyConfigDefaults(cfg)
	return cfg, nil
}

// applyConfigDefaults fills in zero-value fields with sensible defaults.
func applyConfigDefaults(cfg *Config) {
	if cfg.Listen == "" {
		cfg.Listen = ":8080"
	}
	if cfg.DB == "" {
		cfg.DB = "./overseer.db"
	}
}
