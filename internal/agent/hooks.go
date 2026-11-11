package agent

import (
	"context"
	"fmt"
	"log"

	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/config"
	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/state"
)

type Hooks struct {
	Store  *state.Store
	View   ViewReader
	Logger *log.Logger
}

func (hooks Hooks) Change(ctx context.Context) error {
	proposed, err := hooks.View.Read(ctx, false)
	if err != nil {
		return fmt.Errorf("cannot read proposed view: %w", err)
	}
	previous, err := hooks.View.Read(ctx, true)
	if err != nil {
		return fmt.Errorf("cannot read previous view: %w", err)
	}
	newConfig, err := config.Parse(proposed)
	if err != nil {
		return fmt.Errorf("invalid proposed configuration: %w", err)
	}
	if err := newConfig.ValidateUniqueOutputs(); err != nil {
		return fmt.Errorf("invalid proposed configuration: %w", err)
	}
	oldConfig, err := config.Parse(previous)
	if err != nil {
		return fmt.Errorf("invalid previous configuration: %w", err)
	}
	if !config.SourcesEqual(newConfig.Sources, oldConfig.Sources) {
		if err := newConfig.ValidateSourcesAgainstPolicy(); err != nil {
			return fmt.Errorf("proposed sources violate URL policy: %w", err)
		}
	}
	if err := hooks.Store.Bump(); err != nil {
		return fmt.Errorf("cannot invalidate in-flight refresh: %w", err)
	}
	hooks.notify()
	return nil
}

func (hooks Hooks) Observe() {
	hooks.notify()
}

func (hooks Hooks) Connect() {
	hooks.notify()
}

func (hooks Hooks) Disconnect() error {
	if err := hooks.Store.Bump(); err != nil {
		return fmt.Errorf("cannot invalidate refresh after disconnection: %w", err)
	}
	hooks.notify()
	return nil
}

func (hooks Hooks) notify() {
	if err := Notify(hooks.Store); err != nil {
		hooks.Logger.Printf("refresh service was not notified; it will refresh on startup or a later request: %v", err)
	}
}
