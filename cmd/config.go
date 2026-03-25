package cmd

import (
	"fmt"

	"github.com/marccampbell/autoprobe/pkg/config"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:   "config",
	Short: "Configuration management commands",
}

var configLintCmd = &cobra.Command{
	Use:   "lint",
	Short: "Validate the configuration file",
	Long: `Lint and validate the .autoprobe.yaml configuration file.

Checks for:
  - Valid YAML syntax
  - Required fields in endpoints
  - Valid duration formats for targets
  - Undefined variable references
  - Database connection format`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runConfigLint()
	},
}

func init() {
	rootCmd.AddCommand(configCmd)
	configCmd.AddCommand(configLintCmd)
}

func runConfigLint() error {
	cfg, err := config.LoadDefault()
	if err != nil {
		fmt.Printf("✗ %s\n", err)
		return err
	}

	errors := []string{}
	warnings := []string{}

	// Check endpoints
	if len(cfg.Endpoints) == 0 {
		warnings = append(warnings, "No endpoints defined")
	}

	for name, ep := range cfg.Endpoints {
		// Required: URL
		if ep.URL == "" {
			errors = append(errors, fmt.Sprintf("endpoint %q: missing url", name))
		}

		// Check for unexpanded variables in URL
		if hasUnexpandedVars(ep.URL) {
			errors = append(errors, fmt.Sprintf("endpoint %q: url contains undefined variable", name))
		}

		// Check for unexpanded variables in headers
		for hdr, val := range ep.Headers {
			if hasUnexpandedVars(val) {
				errors = append(errors, fmt.Sprintf("endpoint %q: header %q contains undefined variable", name, hdr))
			}
		}

		// Check for unexpanded variables in body
		if hasUnexpandedVars(ep.Body) {
			errors = append(errors, fmt.Sprintf("endpoint %q: body contains undefined variable", name))
		}

		// Default method
		method := ep.Method
		if method == "" {
			method = "GET"
		}

		// Warn if no target set
		if ep.Target.Duration() == 0 {
			warnings = append(warnings, fmt.Sprintf("endpoint %q: no target latency set", name))
		}

		// Warn if POST/PUT/PATCH without body
		if (method == "POST" || method == "PUT" || method == "PATCH") && ep.Body == "" {
			warnings = append(warnings, fmt.Sprintf("endpoint %q: %s request without body", name, method))
		}
	}

	// Check databases
	for name, db := range cfg.Databases {
		if db.Driver == "" {
			errors = append(errors, fmt.Sprintf("database %q: missing driver", name))
		}

		// Must have DSN or host
		if db.DSN == "" && db.Host == "" {
			errors = append(errors, fmt.Sprintf("database %q: missing dsn or host", name))
		}

		// Validate driver
		switch db.Driver {
		case "postgres", "mysql", "sqlite":
			// OK
		case "":
			// Already reported above
		default:
			warnings = append(warnings, fmt.Sprintf("database %q: unknown driver %q", name, db.Driver))
		}
	}

	// Print results
	if len(errors) == 0 && len(warnings) == 0 {
		fmt.Println("✓ Configuration is valid")
		fmt.Printf("  %d endpoint(s), %d database(s), %d variable(s)\n",
			len(cfg.Endpoints), len(cfg.Databases), len(cfg.Variables))
		return nil
	}

	if len(errors) > 0 {
		fmt.Println("Errors:")
		for _, e := range errors {
			fmt.Printf("  ✗ %s\n", e)
		}
	}

	if len(warnings) > 0 {
		fmt.Println("Warnings:")
		for _, w := range warnings {
			fmt.Printf("  ⚠ %s\n", w)
		}
	}

	if len(errors) > 0 {
		return fmt.Errorf("configuration has %d error(s)", len(errors))
	}

	fmt.Println("\n✓ Configuration is valid (with warnings)")
	return nil
}

// hasUnexpandedVars checks if a string contains {{var}} that wasn't expanded
func hasUnexpandedVars(s string) bool {
	// Simple check: if {{ and }} are present, something wasn't expanded
	for i := 0; i < len(s)-3; i++ {
		if s[i] == '{' && s[i+1] == '{' {
			for j := i + 2; j < len(s)-1; j++ {
				if s[j] == '}' && s[j+1] == '}' {
					return true
				}
			}
		}
	}
	return false
}
