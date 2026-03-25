package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the .autoprobe.yaml configuration
type Config struct {
	Databases map[string]DatabaseConfig `yaml:"databases"`
	Endpoints map[string]EndpointConfig `yaml:"endpoints"`
}

// DatabaseConfig represents a database connection
type DatabaseConfig struct {
	Driver   string `yaml:"driver"`
	DSN      string `yaml:"dsn"`
	Host     string `yaml:"host"`
	Port     int    `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Database string `yaml:"database"`
}

// EndpointConfig represents an endpoint to optimize
type EndpointConfig struct {
	URL     string            `yaml:"url"`
	Method  string            `yaml:"method"`
	Target  Duration          `yaml:"target"`
	Headers map[string]string `yaml:"headers"`
	Body    string            `yaml:"body"`
}

// Duration is a wrapper for time.Duration that supports YAML unmarshaling
type Duration time.Duration

func (d *Duration) UnmarshalYAML(value *yaml.Node) error {
	var s string
	if err := value.Decode(&s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(parsed)
	return nil
}

func (d Duration) Duration() time.Duration {
	return time.Duration(d)
}

// Load reads and parses the config file
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config: %w", err)
	}

	// Expand environment variables
	expanded := os.ExpandEnv(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	return &cfg, nil
}

// LoadDefault loads config from .autoprobe.yaml in the current directory
func LoadDefault() (*Config, error) {
	return Load(".autoprobe.yaml")
}

// GetEndpoint returns the endpoint config by name
func (c *Config) GetEndpoint(name string) (*EndpointConfig, error) {
	ep, ok := c.Endpoints[name]
	if !ok {
		return nil, fmt.Errorf("endpoint %q not found in config", name)
	}
	return &ep, nil
}

// GetDatabase returns the database config by name
func (c *Config) GetDatabase(name string) (*DatabaseConfig, error) {
	db, ok := c.Databases[name]
	if !ok {
		return nil, fmt.Errorf("database %q not found in config", name)
	}
	return &db, nil
}
