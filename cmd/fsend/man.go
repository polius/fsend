//go:build fsend_man

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/cobra/doc"
	"github.com/spf13/pflag"

	"github.com/polius/fsend/internal/version"
)

// The release hook builds with -tags fsend_man and runs
// `fsend __man <dir>` to write the man pages (fsend.1 and one per
// subcommand). Release binaries don't carry this command or cobra/doc.
func init() { buildTagCmds = append(buildTagCmds, manCmd) }

func manCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "__man <dir>",
		Short:  "write the man pages to <dir>",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if err := os.MkdirAll(args[0], 0o755); err != nil {
				return err
			}
			// SOURCE_DATE_EPOCH (the release commit's time) keeps the pages
			// reproducible; cobra/doc would stamp today's date otherwise.
			date := time.Now()
			if s := os.Getenv("SOURCE_DATE_EPOCH"); s != "" {
				sec, err := strconv.ParseInt(s, 10, 64)
				if err != nil {
					return fmt.Errorf("SOURCE_DATE_EPOCH %q: %w", s, err)
				}
				date = time.Unix(sec, 0).UTC()
			}
			escapePlaceholders(c.Root())
			return doc.GenManTree(c.Root(), &doc.GenManHeader{
				Title:   "FSEND",
				Section: "1",
				Date:    &date,
				Source:  "fsend " + version.Version,
				Manual:  "fsend manual",
			}, args[0])
		},
	}
}

// escapePlaceholders keeps "<host:port>"-style placeholders in the pages:
// cobra/doc renders through Markdown, which drops them as HTML tags.
func escapePlaceholders(c *cobra.Command) {
	esc := strings.NewReplacer("<", `\<`, ">", `\>`)
	c.Long, c.Use = esc.Replace(c.Long), esc.Replace(c.Use)
	c.Flags().VisitAll(func(f *pflag.Flag) { f.Usage = esc.Replace(f.Usage) })
	for _, sub := range c.Commands() {
		escapePlaceholders(sub)
	}
}
