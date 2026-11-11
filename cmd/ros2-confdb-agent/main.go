package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/agent"
	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/fetch"
	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/state"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ros2-confdb-agent:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("expected one command: init, daemon, refresh, change-hook, observe-hook, connect-hook, or disconnect-hook")
	}
	store, err := state.Open(os.Getenv("SNAP_COMMON"))
	if err != nil {
		return err
	}
	logger := log.New(os.Stderr, "ros2-confdb-agent: ", log.LstdFlags)
	view := agent.SnapctlView{Plug: "ros2-agent"}
	hooks := agent.Hooks{Store: store, View: view, Logger: logger}
	switch args[0] {
	case "init":
		return nil
	case "daemon":
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		reconciler := &agent.Reconciler{
			Store:   store,
			View:    view,
			Fetcher: fetch.Downloader{},
			Logger:  logger,
		}
		return reconciler.Serve(ctx)
	case "refresh":
		if err := agent.Notify(store); err != nil {
			return err
		}
		fmt.Println("Refresh requested; check the daemon log for the result.")
		return nil
	case "change-hook":
		ctx := context.Background()
		if err := hooks.Change(ctx); err != nil {
			logger.Printf("rejecting confdb change: %v", err)
			if rejectErr := agent.FailHook(ctx, err.Error()); rejectErr != nil {
				return fmt.Errorf("cannot reject confdb change: %w", rejectErr)
			}
		}
		return nil
	case "observe-hook":
		hooks.Observe()
		return nil
	case "connect-hook":
		hooks.Connect()
		return nil
	case "disconnect-hook":
		return hooks.Disconnect()
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}
