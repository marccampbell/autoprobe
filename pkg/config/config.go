package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config represents the .autoprobe.yaml configuration
type Config struct {
	Variables map[string]string         `yaml:"variables"`
	Databases map[string]DatabaseConfig `yaml:"databases"`
	Endpoints map[string]EndpointConfig `yaml:"endpoints"`
	Pages     map[string]PageConfig     `yaml:"pages"`
	Rules     string                    `yaml:"rules"`
}

// PageConfig represents a page to benchmark (browser-based)
type PageConfig struct {
	URL       string            `yaml:"url"`
	Cookies   []CookieConfig    `yaml:"cookies"`
	LocalStorage map[string]string `yaml:"localStorage"`
	Headers   map[string]string `yaml:"headers"`
	WaitFor   string            `yaml:"wait_for"` // "networkidle", "load", "domcontentloaded", or selector
}

// CookieConfig represents a browser cookie
type CookieConfig struct {
	Name     string `yaml:"name"`
	Value    string `yaml:"value"`
	Domain   string `yaml:"domain"`
	Path     string `yaml:"path"`
	Secure   bool   `yaml:"secure"`
	HttpOnly bool   `yaml:"httpOnly"`
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
	URL        string            `yaml:"url"`
	Method     string            `yaml:"method"`
	Target     Duration          `yaml:"target"`
	Headers    map[string]string `yaml:"headers"`
	Body       string            `yaml:"body"`
	Expect     int               `yaml:"expect"`     // Expected status code (default: 2xx)
	Requests   int               `yaml:"requests"`   // Override default request count
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

	// Expand environment variables (${VAR} syntax)
	expanded := os.ExpandEnv(string(data))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("failed to parse config: %w", err)
	}

	// Expand template variables ({{var}} syntax)
	cfg.expandVariables()

	return &cfg, nil
}

// expandVariables replaces {{var}} placeholders with values from the variables section
func (c *Config) expandVariables() {
	if len(c.Variables) == 0 {
		return
	}

	// Pattern matches {{variable_name}}
	pattern := regexp.MustCompile(`\{\{\s*(\w+)\s*\}\}`)

	expand := func(s string) string {
		return pattern.ReplaceAllStringFunc(s, func(match string) string {
			// Extract variable name
			submatch := pattern.FindStringSubmatch(match)
			if len(submatch) < 2 {
				return match
			}
			varName := strings.TrimSpace(submatch[1])
			if val, ok := c.Variables[varName]; ok {
				return val
			}
			return match // Leave unchanged if not found
		})
	}

	// Expand in endpoints
	for name, ep := range c.Endpoints {
		ep.URL = expand(ep.URL)
		ep.Body = expand(ep.Body)
		for k, v := range ep.Headers {
			ep.Headers[k] = expand(v)
		}
		c.Endpoints[name] = ep
	}

	// Expand in databases
	for name, db := range c.Databases {
		db.DSN = expand(db.DSN)
		db.Host = expand(db.Host)
		db.User = expand(db.User)
		db.Password = expand(db.Password)
		db.Database = expand(db.Database)
		c.Databases[name] = db
	}
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

// GetPage returns the page config by name
func (c *Config) GetPage(name string) (*PageConfig, error) {
	pg, ok := c.Pages[name]
	if !ok {
		return nil, fmt.Errorf("page %q not found in config", name)
	}
	return &pg, nil
}

// IsPage checks if a name refers to a page (vs endpoint)
func (c *Config) IsPage(name string) bool {
	_, ok := c.Pages[name]
	return ok
}

// GetDatabase returns the database config by name
func (c *Config) GetDatabase(name string) (*DatabaseConfig, error) {
	db, ok := c.Databases[name]
	if !ok {
		return nil, fmt.Errorf("database %q not found in config", name)
	}
	return &db, nil
}
