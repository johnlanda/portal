package cli

import (
	"fmt"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// NewListCmd creates the `portal list` command.
func NewListCmd() *cobra.Command {
	var outputJSON bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List all known tunnels",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runList(cmd, outputJSON)
		},
	}

	cmd.Flags().BoolVar(&outputJSON, "json", false, "Output in JSON format")

	return cmd
}

func runList(cmd *cobra.Command, outputJSON bool) error {
	store, err := newStateStore()
	if err != nil {
		return fmt.Errorf("failed to initialize state store: %w", err)
	}

	tunnels, err := store.List()
	if err != nil {
		return fmt.Errorf("failed to load tunnel state: %w", err)
	}

	out := cmd.OutOrStdout()

	if len(tunnels) == 0 {
		fmt.Fprintln(out, "No tunnels found.")
		return nil
	}

	if outputJSON {
		return printJSON(out, tunnels)
	}

	// Show TARGET column only when bare-metal tunnels exist.
	hasBM := false
	for _, t := range tunnels {
		if t.DeployTarget == "bare-metal" {
			hasBM = true
			break
		}
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if hasBM {
		fmt.Fprintln(w, "NAME\tSOURCE\tDESTINATION\tNAMESPACE\tPORT\tTARGET\tAGE")
	} else {
		fmt.Fprintln(w, "NAME\tSOURCE\tDESTINATION\tNAMESPACE\tPORT\tAGE")
	}
	for _, t := range tunnels {
		age := formatAge(t.CreatedAt)
		if hasBM {
			target := t.DeployTarget
			if target == "" {
				target = "kubernetes"
			}
			ns := t.Namespace
			if ns == "" {
				ns = "-"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
				t.Name,
				t.SourceContext,
				t.DestinationContext,
				ns,
				t.TunnelPort,
				target,
				age,
			)
		} else {
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n",
				t.Name,
				t.SourceContext,
				t.DestinationContext,
				t.Namespace,
				t.TunnelPort,
				age,
			)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("failed to flush output: %w", err)
	}
	return nil
}
