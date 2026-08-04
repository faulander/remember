// Command remember-dev is a headless manual-test harness for the local core.
// It is not a production client or stable user-facing CLI.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/faulander/remember/client/internal/app"
	"github.com/faulander/remember/client/internal/localindex"
	"github.com/faulander/remember/client/internal/reconcile"
)

type output struct {
	Event   string              `json:"event"`
	Hint    string              `json:"hint,omitempty"`
	Path    string              `json:"path,omitempty"`
	Report  *reconcile.Report   `json:"report,omitempty"`
	Objects []localindex.Object `json:"objects,omitempty"`
	Issues  []localindex.Issue  `json:"issues,omitempty"`
}

func main() {
	if len(os.Args) != 3 {
		usage()
	}
	command, root := os.Args[1], os.Args[2]
	ctx := context.Background()

	switch command {
	case "init":
		core, report, err := app.Initialize(ctx, root)
		if err != nil {
			fatal(err)
		}
		defer core.Close()
		printState(ctx, core, "initialized", "", "", &report)
	case "status":
		core, report, err := app.Open(ctx, root)
		if err != nil {
			fatal(err)
		}
		defer core.Close()
		printState(ctx, core, "status", "", "", &report)
	case "watch":
		watch(root)
	default:
		usage()
	}
}

func watch(root string) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	core, report, err := app.Open(ctx, root)
	if err != nil {
		fatal(err)
	}
	defer core.Close()
	printState(ctx, core, "opened", "", "", &report)

	updates, err := core.StartWatching(ctx)
	if err != nil {
		fatal(err)
	}
	for update := range updates {
		if update.Err != nil && !errors.Is(update.Err, context.Canceled) {
			fmt.Fprintf(os.Stderr, "reconcile error after %s %q: %v\n", update.Hint.Kind, update.Hint.RelativePath, update.Err)
			continue
		}
		if update.Err == nil {
			printState(context.Background(), core, "reconciled", string(update.Hint.Kind), update.Hint.RelativePath, &update.Report)
		}
	}
}

func printState(ctx context.Context, core *app.LocalCore, event, hint, path string, report *reconcile.Report) {
	snapshot, err := core.Snapshot(ctx)
	if err != nil {
		fatal(err)
	}
	encoded, err := json.MarshalIndent(output{
		Event: event, Hint: hint, Path: path, Report: report,
		Objects: snapshot.Objects, Issues: snapshot.Issues,
	}, "", "  ")
	if err != nil {
		fatal(err)
	}
	fmt.Println(string(encoded))
}

func usage() {
	fmt.Fprintf(os.Stderr, "usage: %s <init|status|watch> <root-directory>\n", os.Args[0])
	os.Exit(2)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "remember-dev:", err)
	os.Exit(1)
}
