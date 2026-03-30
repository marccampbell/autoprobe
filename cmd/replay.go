package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
)

// DiscardedAttempt stores info about the last discarded optimization
type DiscardedAttempt struct {
	CommitHash     string   `json:"commitHash"`
	Hypothesis     string   `json:"hypothesis"`
	Change         string   `json:"change"`
	FilesChanged   []string `json:"filesChanged"`
	Reason         string   `json:"reason"`
	ScreenshotPath string   `json:"screenshotPath"`
	BaselineScreenshot string `json:"baselineScreenshot"`
	ConsoleErrors  []string `json:"consoleErrors"`
}

var replayCmd = &cobra.Command{
	Use:   "replay",
	Short: "Re-apply the last discarded optimization for inspection",
	Long: `Cherry-picks the last discarded optimization back into your working tree
so you can inspect what went wrong.

Use 'autoprobe revert' to undo when done.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runReplay()
	},
}

var revertCmd = &cobra.Command{
	Use:   "revert",
	Short: "Undo a replayed optimization",
	Long:  `Resets the working tree to HEAD, undoing any replayed changes.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runRevert()
	},
}

func init() {
	rootCmd.AddCommand(replayCmd)
	rootCmd.AddCommand(revertCmd)
}

func getStateFilePath() (string, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("not in a git repository")
	}
	repoRoot := strings.TrimSpace(string(output))
	return filepath.Join(repoRoot, ".autoprobe-state.json"), nil
}

func runReplay() error {
	statePath, err := getStateFilePath()
	if err != nil {
		return err
	}

	data, err := os.ReadFile(statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("no discarded attempt found — run 'autoprobe run' first")
		}
		return err
	}

	var attempt DiscardedAttempt
	if err := json.Unmarshal(data, &attempt); err != nil {
		return fmt.Errorf("invalid state file: %w", err)
	}

	if attempt.CommitHash == "" {
		return fmt.Errorf("no discarded attempt found")
	}

	// Check for uncommitted changes
	cmd := exec.Command("git", "status", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(output))) > 0 {
		return fmt.Errorf("working tree has uncommitted changes — commit or stash first")
	}

	// Cherry-pick the discarded commit
	fmt.Printf("Replaying discarded optimization:\n")
	fmt.Printf("  Hypothesis: %s\n", attempt.Hypothesis)
	fmt.Printf("  Change: %s\n", attempt.Change)
	fmt.Printf("  Files: %s\n", strings.Join(attempt.FilesChanged, ", "))
	fmt.Printf("  Discard reason: %s\n", attempt.Reason)
	fmt.Println()

	cmd = exec.Command("git", "cherry-pick", "--no-commit", attempt.CommitHash)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("cherry-pick failed: %w\n%s", err, output)
	}

	fmt.Println("✓ Changes applied to working tree")
	fmt.Println()
	fmt.Println("Inspect with:")
	fmt.Println("  git diff --cached")
	fmt.Println()
	if attempt.ScreenshotPath != "" {
		fmt.Printf("Screenshots saved:\n")
		fmt.Printf("  Baseline: %s\n", attempt.BaselineScreenshot)
		fmt.Printf("  After:    %s\n", attempt.ScreenshotPath)
	}
	if len(attempt.ConsoleErrors) > 0 {
		fmt.Printf("\nConsole errors detected:\n")
		for _, e := range attempt.ConsoleErrors {
			if len(e) > 100 {
				e = e[:100] + "..."
			}
			fmt.Printf("  • %s\n", e)
		}
	}
	fmt.Println()
	fmt.Println("When done: autoprobe revert")

	return nil
}

func runRevert() error {
	// Check if there are staged changes
	cmd := exec.Command("git", "diff", "--cached", "--name-only")
	output, err := cmd.Output()
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(output))) == 0 {
		fmt.Println("Nothing to revert")
		return nil
	}

	cmd = exec.Command("git", "reset", "--hard", "HEAD")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("reset failed: %w\n%s", err, output)
	}

	fmt.Println("✓ Reverted to HEAD")
	return nil
}

// SaveDiscardedAttempt saves info about the last discarded attempt for replay
func SaveDiscardedAttempt(attempt DiscardedAttempt) error {
	statePath, err := getStateFilePath()
	if err != nil {
		return err
	}

	data, err := json.MarshalIndent(attempt, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(statePath, data, 0644)
}
