package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Tool represents a tool definition for the Claude API
type Tool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"input_schema"`
}

// ToolUse represents a tool call from Claude
type ToolUse struct {
	ID    string                 `json:"id"`
	Name  string                 `json:"name"`
	Input map[string]interface{} `json:"input"`
}

// ToolResult represents the result of executing a tool
type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error,omitempty"`
}

// GetTools returns the available tools for code analysis
func GetTools(allowWrite bool) []Tool {
	tools := []Tool{
		{
			Name:        "read_file",
			Description: "Read the contents of a file at the specified path. Returns the file content as a string.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "The path to the file to read (relative to working directory)",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "list_files",
			Description: "List files and directories at the specified path. Returns a list of file/directory names.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "The directory path to list (relative to working directory)",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "glob",
			Description: "Find files matching a glob pattern. Use 'pattern' parameter with glob syntax like '**/*.tsx' or 'src/**/*.ts'.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pattern": map[string]interface{}{
						"type":        "string",
						"description": "Glob pattern (required). Examples: '**/*.go', 'src/**/*.ts', 'vendor-web/**/*.tsx'",
					},
				},
				"required": []string{"pattern"},
			},
		},
		{
			Name:        "grep",
			Description: "Search for a pattern in files. Returns matching lines with file paths and line numbers.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"pattern": map[string]interface{}{
						"type":        "string",
						"description": "The regex pattern to search for",
					},
					"path": map[string]interface{}{
						"type":        "string",
						"description": "Directory or file to search in (default: current directory)",
					},
					"include": map[string]interface{}{
						"type":        "string",
						"description": "File pattern to include (e.g., '*.go')",
					},
				},
				"required": []string{"pattern"},
			},
		},
	}

	if allowWrite {
		tools = append(tools, Tool{
			Name:        "write_file",
			Description: "Write content to a file. Creates the file if it doesn't exist, overwrites if it does.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "The path to write to (relative to working directory)",
					},
					"content": map[string]interface{}{
						"type":        "string",
						"description": "The content to write to the file",
					},
				},
				"required": []string{"path", "content"},
			},
		})

		tools = append(tools, Tool{
			Name:        "edit_file",
			Description: "Edit a file by replacing a specific string with new content. Use for surgical edits.",
			InputSchema: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"path": map[string]interface{}{
						"type":        "string",
						"description": "The path to the file to edit",
					},
					"old_string": map[string]interface{}{
						"type":        "string",
						"description": "The exact string to find and replace",
					},
					"new_string": map[string]interface{}{
						"type":        "string",
						"description": "The string to replace it with",
					},
				},
				"required": []string{"path", "old_string", "new_string"},
			},
		})
	}

	return tools
}

// ExecuteTool runs a tool and returns the result
func ExecuteTool(tool ToolUse) ToolResult {
	var content string
	var isError bool

	switch tool.Name {
	case "read_file":
		content, isError = executeReadFile(tool.Input)
	case "list_files":
		content, isError = executeListFiles(tool.Input)
	case "glob":
		content, isError = executeGlob(tool.Input)
	case "grep":
		content, isError = executeGrep(tool.Input)
	case "write_file":
		content, isError = executeWriteFile(tool.Input)
	case "edit_file":
		content, isError = executeEditFile(tool.Input)
	default:
		content = fmt.Sprintf("Unknown tool: %s", tool.Name)
		isError = true
	}

	return ToolResult{
		ToolUseID: tool.ID,
		Content:   content,
		IsError:   isError,
	}
}

func executeReadFile(input map[string]interface{}) (string, bool) {
	path, ok := input["path"].(string)
	if !ok {
		return "path is required", true
	}

	// Block path traversal
	if strings.Contains(path, "..") {
		return "path cannot contain '..'", true
	}

	// Block reading sensitive files
	lowerPath := strings.ToLower(path)
	blockedPatterns := []string{".env", "secret", "password", "credential", ".git/"}
	for _, pattern := range blockedPatterns {
		if strings.Contains(lowerPath, pattern) {
			return fmt.Sprintf("cannot read files matching '%s'", pattern), true
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("Error reading file: %v", err), true
	}

	content := string(data)
	// Truncate very large files
	if len(content) > 50000 {
		return content[:50000] + "\n... (file truncated, too large)", false
	}

	return content, false
}

func executeListFiles(input map[string]interface{}) (string, bool) {
	path, ok := input["path"].(string)
	if !ok {
		path = "."
	}

	entries, err := os.ReadDir(path)
	if err != nil {
		return fmt.Sprintf("Error listing directory: %v", err), true
	}

	var lines []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		lines = append(lines, name)
	}

	return strings.Join(lines, "\n"), false
}

func executeGlob(input map[string]interface{}) (string, bool) {
	pattern, ok := input["pattern"].(string)
	if !ok {
		// Fallback: try "path" if model used wrong param name
		pattern, ok = input["path"].(string)
		if !ok {
			return "pattern is required", true
		}
	}

	// Use find for recursive globs since Go's filepath.Glob doesn't support **
	if strings.Contains(pattern, "**") {
		// Convert glob to find command
		ext := filepath.Ext(pattern)
		cmd := exec.Command("find", ".", "-type", "f", "-name", "*"+ext)
		output, err := cmd.Output()
		if err != nil {
			return fmt.Sprintf("Error executing glob: %v", err), true
		}
		return strings.TrimSpace(string(output)), false
	}

	matches, err := filepath.Glob(pattern)
	if err != nil {
		return fmt.Sprintf("Error executing glob: %v", err), true
	}

	return strings.Join(matches, "\n"), false
}

func executeGrep(input map[string]interface{}) (string, bool) {
	pattern, ok := input["pattern"].(string)
	if !ok {
		return "pattern is required", true
	}

	// Block dangerous patterns
	if strings.Contains(pattern, "..") {
		return "pattern cannot contain '..'", true
	}

	path := "."
	if p, ok := input["path"].(string); ok {
		// Block path traversal
		if strings.Contains(p, "..") {
			return "path cannot contain '..'", true
		}
		path = p
	}

	args := []string{"-r", "-n", "--max-count=100", pattern, path}

	if include, ok := input["include"].(string); ok {
		args = []string{"-r", "-n", "--max-count=100", "--include", include, pattern, path}
	}

	cmd := exec.Command("grep", args...)
	output, err := cmd.Output()
	if err != nil {
		// grep returns exit code 1 if no matches found
		if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 {
			return "No matches found", false
		}
		return fmt.Sprintf("Error executing grep: %v", err), true
	}

	result := strings.TrimSpace(string(output))
	// Truncate if too long
	if len(result) > 8000 {
		result = result[:8000] + "\n... (truncated, use more specific pattern)"
	}

	return result, false
}

func executeWriteFile(input map[string]interface{}) (string, bool) {
	path, ok := input["path"].(string)
	if !ok {
		return "path is required", true
	}

	content, ok := input["content"].(string)
	if !ok {
		return "content is required", true
	}

	// Create parent directories if needed
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Sprintf("Error creating directory: %v", err), true
		}
	}

	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		return fmt.Sprintf("Error writing file: %v", err), true
	}

	return fmt.Sprintf("Successfully wrote %d bytes to %s", len(content), path), false
}

func executeEditFile(input map[string]interface{}) (string, bool) {
	path, ok := input["path"].(string)
	if !ok {
		return "path is required", true
	}

	oldString, ok := input["old_string"].(string)
	if !ok {
		return "old_string is required", true
	}

	newString, ok := input["new_string"].(string)
	if !ok {
		return "new_string is required", true
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("Error reading file: %v", err), true
	}

	content := string(data)
	if !strings.Contains(content, oldString) {
		return "old_string not found in file", true
	}

	// Count occurrences
	count := strings.Count(content, oldString)
	if count > 1 {
		return fmt.Sprintf("old_string found %d times, expected exactly 1. Be more specific.", count), true
	}

	newContent := strings.Replace(content, oldString, newString, 1)

	if err := os.WriteFile(path, []byte(newContent), 0644); err != nil {
		return fmt.Sprintf("Error writing file: %v", err), true
	}

	return fmt.Sprintf("Successfully edited %s", path), false
}

// ToJSON converts tools to JSON for API request
func ToJSON(tools []Tool) ([]byte, error) {
	return json.Marshal(tools)
}
