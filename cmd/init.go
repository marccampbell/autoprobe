package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Create .autoprobe.yaml in the current directory",
	Long: `Initialize a new autoprobe configuration file.

Creates a .autoprobe.yaml file with example endpoints and database
configuration that you can customize for your project.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runInit()
	},
}

func init() {
	rootCmd.AddCommand(initCmd)
}

func runInit() error {
	configPath := ".autoprobe.yaml"

	// Check if config already exists
	if _, err := os.Stat(configPath); err == nil {
		return fmt.Errorf("%s already exists", configPath)
	}

	template := `# autoprobe configuration
# See https://github.com/marccampbell/autoprobe for documentation

# Variables for use in endpoint definitions
# variables:
#   base_url: http://localhost:8080
#   api_token: ${API_TOKEN}

# Database connections for query analysis (optional)
# databases:
#   primary:
#     driver: postgres
#     dsn: ${DATABASE_URL}

# Rules for the AI optimizer (optional)
# rules: |
#   - Do not add new dependencies
#   - Do not modify database schema
#   - Only touch files in pkg/ and internal/

# Endpoint definitions
endpoints:
  example:
    url: http://localhost:8080/api/example
    method: GET
    # expect: 200
    # target: 200ms
    # headers:
    #   Authorization: Bearer {{api_token}}
`

	if err := os.WriteFile(configPath, []byte(template), 0644); err != nil {
		return fmt.Errorf("failed to write config: %w", err)
	}

	fmt.Printf("Created %s\n", configPath)
	fmt.Println("\nNext steps:")
	fmt.Println("  1. Edit .autoprobe.yaml to add your endpoints")
	fmt.Println("  2. Run: autoprobe benchmark <endpoint-name>")
	fmt.Println("  3. Run: autoprobe run <endpoint-name>")

	return nil
}
