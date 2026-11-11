package agent

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/config"
	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/state"
)

type ViewReader interface {
	Read(context.Context, bool) ([]byte, error)
}

type SourceFetcher interface {
	Fetch(context.Context, string, config.Config) ([]byte, error)
}

type Reconciler struct {
	Store       *state.Store
	View        ViewReader
	Fetcher     SourceFetcher
	Logger      *log.Logger
	RetryDelays []time.Duration
}

const maxOutputBytes = 64 * 1024 * 1024

func (reconciler *Reconciler) Reconcile(ctx context.Context) error {
	delays := reconciler.retryDelays()
	cfg, generation, err := reconciler.readSnapshot(ctx, delays)
	if err != nil {
		return err
	}
	if err := cfg.ValidateUniqueOutputs(); err != nil {
		return fmt.Errorf("invalid confdb configuration: %w", err)
	}
	desired := make(map[string]bool, len(cfg.Sources))
	for _, source := range cfg.Sources {
		desired[source.Output] = true
	}
	pending := cfg.Sources
	cleanupPending := true
	for attempt := 0; attempt <= len(delays); attempt++ {
		if attempt > 0 {
			if err := wait(ctx, delays[attempt-1]); err != nil {
				return err
			}
		}
		if cleanupPending {
			err := reconciler.Store.RemoveUnconfigured(generation, desired)
			if errors.Is(err, state.ErrStale) {
				return err
			}
			if err != nil {
				reconciler.Logger.Printf("removing unconfigured outputs, attempt %d/%d: %s",
					attempt+1, len(delays)+1, config.RedactSecrets(err.Error(), cfg.Credentials))
			} else {
				cleanupPending = false
			}
		}
		next := make([]config.Source, 0, len(pending))
		for _, source := range pending {
			if err := ctx.Err(); err != nil {
				return err
			}
			err := reconciler.refreshSource(ctx, cfg, generation, source)
			if errors.Is(err, state.ErrStale) || ctx.Err() != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				return err
			}
			if err != nil {
				reconciler.Logger.Printf("refreshing output %q, attempt %d/%d: %s",
					config.RedactSecrets(source.Output, cfg.Credentials), attempt+1, len(delays)+1,
					config.RedactSecrets(err.Error(), cfg.Credentials))
				next = append(next, source)
				continue
			}
			reconciler.Logger.Printf("published output %q", config.RedactSecrets(source.Output, cfg.Credentials))
		}
		pending = next
		if !cleanupPending && len(pending) == 0 {
			return nil
		}
	}
	return fmt.Errorf("refresh failed after %d attempts; see earlier errors", len(delays)+1)
}

func (reconciler *Reconciler) readSnapshot(ctx context.Context, delays []time.Duration) (config.Config, uint64, error) {
	for attempt := 0; attempt <= len(delays); attempt++ {
		if attempt > 0 {
			if err := wait(ctx, delays[attempt-1]); err != nil {
				return config.Config{}, 0, err
			}
		}
		cfg, generation, err := reconciler.readStableSnapshot(ctx)
		if err == nil {
			return cfg, generation, nil
		}
		if ctx.Err() != nil {
			return config.Config{}, 0, ctx.Err()
		}
		reconciler.Logger.Printf("reading confdb configuration, attempt %d/%d: %v", attempt+1, len(delays)+1, err)
	}
	return config.Config{}, 0, fmt.Errorf("cannot read confdb configuration after %d attempts", len(delays)+1)
}

func (reconciler *Reconciler) readStableSnapshot(ctx context.Context) (config.Config, uint64, error) {
	for {
		if err := ctx.Err(); err != nil {
			return config.Config{}, 0, err
		}
		before, err := reconciler.Store.Generation()
		if err != nil {
			return config.Config{}, 0, err
		}
		data, err := reconciler.View.Read(ctx, false)
		if err != nil {
			return config.Config{}, 0, err
		}
		after, err := reconciler.Store.Generation()
		if err != nil {
			return config.Config{}, 0, err
		}
		if before != after {
			continue
		}
		cfg, err := config.Parse(data)
		if err != nil {
			return config.Config{}, 0, err
		}
		return cfg, after, nil
	}
}

func (reconciler *Reconciler) refreshSource(ctx context.Context, cfg config.Config, generation uint64, source config.Source) error {
	data, err := reconciler.Fetcher.Fetch(ctx, source.URL, cfg)
	if err != nil {
		return fmt.Errorf("cannot download source: %w", err)
	}
	if len(data) > maxOutputBytes {
		return fmt.Errorf("source exceeds the %d-byte per-output limit", maxOutputBytes)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return reconciler.Store.Publish(generation, source.Output, data)
}

func (reconciler *Reconciler) retryDelays() []time.Duration {
	if reconciler.RetryDelays == nil {
		return []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second}
	}
	return reconciler.RetryDelays
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
