package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	tea "charm.land/bubbletea/v2"
	"charm.land/log/v2"
	zone "github.com/lrstanley/bubblezone/v2"

	"github.com/dlvhdr/diffnav/pkg/config"
	"github.com/dlvhdr/diffnav/pkg/ui"
	"github.com/dlvhdr/diffnav/pkg/watch"
)

var difftCmd = &cobra.Command{
	Use:   "difft",
	Short: "View diffs using difftastic (structural diff)",
	Long:  "Use difftastic (difft) for structural, syntax-aware diff rendering instead of delta.",
	Example: `# structural diff of unstaged changes
diffnav difft

# structural diff of staged changes
diffnav difft --cmd "git diff --cached"

# structural diff against a branch
diffnav difft --cmd "git diff main..."

# watch mode with structural diffs
diffnav difft --watch --cmd "git diff HEAD"
	`,
	Run: func(cmd *cobra.Command, args []string) {
		if _, err := exec.LookPath("difft"); err != nil {
			fmt.Fprintln(os.Stderr, "difftastic (difft) not found in PATH.")
			fmt.Fprintln(os.Stderr, "Install from: https://difftastic.wilfred.me.uk/")
			os.Exit(1)
		}

		gitCmd, err := cmd.Flags().GetString("cmd")
		if err != nil {
			log.Fatal("Cannot parse the cmd flag", err)
		}
		sideBySideFlag, err := cmd.Flags().GetBool("side-by-side")
		if err != nil {
			log.Fatal("Cannot parse the side-by-side flag", err)
		}
		showBothFlag, err := cmd.Flags().GetBool("show-both")
		if err != nil {
			log.Fatal("Cannot parse the show-both flag", err)
		}
		unifiedFlag, err := cmd.Flags().GetBool("unified")
		if err != nil {
			log.Fatal("Cannot parse the unified flag", err)
		}
		watchFlag, err := cmd.Flags().GetBool("watch")
		if err != nil {
			log.Fatal("Cannot parse the watch flag", err)
		}
		watchInterval, err := cmd.Flags().GetDuration("watch-interval")
		if err != nil {
			log.Fatal("Cannot parse the watch-interval flag", err)
		}

		zone.NewGlobal()

		defer setupLogging()()

		// Warn if stdin is piped (difft mode runs its own git command)
		stat, sErr := os.Stdin.Stat()
		if sErr == nil && stat.Mode()&os.ModeNamedPipe != 0 {
			fmt.Fprintln(os.Stderr, "Warning: stdin input ignored in difft mode")
		}

		// Run the git command to get unified diff for file tree parsing
		input, wErr := watch.RunCmd(gitCmd)
		if wErr != nil {
			log.Warn("initial git command failed, starting with empty diff", "err", wErr)
		}

		if !watchFlag && strings.TrimSpace(input) == "" {
			fmt.Println("No diff, exiting")
			os.Exit(0)
		}

		cfg := config.Load()

		cfg.Difft = config.DifftConfig{
			Enabled: true,
			GitCmd:  gitCmd,
		}

		if unifiedFlag {
			cfg.UI.DifftDisplay = "inline"
		} else if showBothFlag {
			cfg.UI.DifftDisplay = "side-by-side-show-both"
		} else if sideBySideFlag {
			cfg.UI.DifftDisplay = "side-by-side"
		}

		cfg.Watch = config.WatchConfig{
			Enabled:  watchFlag,
			Cmd:      gitCmd,
			Interval: watchInterval,
		}

		ttyIn, _, err := tea.OpenTTY()
		if err != nil {
			log.Fatal(err)
		}
		p := tea.NewProgram(ui.New(input, cfg), tea.WithInput(ttyIn))

		if _, err := p.Run(); err != nil {
			log.Fatal(err)
		}
	},
}

func init() {
	difftCmd.Flags().String("cmd", "git diff", "Git diff command to run")
	difftCmd.Flags().BoolP("side-by-side", "s", false, "Force side-by-side diff view")
	difftCmd.Flags().BoolP("show-both", "b", false, "Force side-by-side-show-both diff view (always two columns)")
	difftCmd.Flags().BoolP("unified", "u", false, "Force unified (inline) diff view")
	difftCmd.Flags().BoolP("watch", "w", false, "Watch mode: periodically re-run the diff command and refresh")
	difftCmd.Flags().Duration("watch-interval", 2*time.Second, "Interval between watch refreshes")

	rootCmd.AddCommand(difftCmd)
}
