package agent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/mehranrahmanzadeh/ros2-confdb-poc/internal/state"
)

const refreshMessage = "refresh\n"

func (reconciler *Reconciler) Serve(ctx context.Context) (err error) {
	lock, err := reconciler.Store.LockDaemon()
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := lock.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("cannot release daemon lock: %w", closeErr))
		}
	}()
	if err := reconciler.Store.CleanupTemporary(); err != nil {
		return err
	}

	socket := reconciler.Store.SocketPath()
	info, statErr := os.Lstat(socket)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return fmt.Errorf("cannot inspect refresh socket: %w", statErr)
	}
	if statErr == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("refresh socket path is not a socket")
		}
		if err := os.Remove(socket); err != nil {
			return fmt.Errorf("cannot remove stale refresh socket: %w", err)
		}
	}
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("cannot listen for refresh requests: %w", err)
	}
	defer func() {
		if closeErr := listener.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("cannot close refresh socket: %w", closeErr))
		}
	}()
	if err := os.Chmod(socket, 0600); err != nil {
		return fmt.Errorf("cannot secure refresh socket: %w", err)
	}

	requests := make(chan struct{}, 1)
	socketErrors := make(chan error, 1)
	go func() {
		buffer := make([]byte, 64)
		for {
			n, _, readErr := listener.ReadFromUnix(buffer)
			if readErr != nil {
				if !errors.Is(readErr, net.ErrClosed) {
					socketErrors <- fmt.Errorf("cannot receive refresh request: %w", readErr)
				}
				return
			}
			if string(buffer[:n]) != refreshMessage {
				reconciler.Logger.Printf("ignored invalid refresh request")
				continue
			}
			select {
			case requests <- struct{}{}:
			default:
			}
		}
	}()

	var done <-chan error
	var cancel context.CancelFunc
	pending := false
	start := func() {
		var runCtx context.Context
		runCtx, cancel = context.WithCancel(ctx)
		result := make(chan error, 1)
		done = result
		pending = false
		go func() { result <- reconciler.Reconcile(runCtx) }()
	}
	start()
	for {
		select {
		case <-ctx.Done():
			if cancel != nil {
				cancel()
				<-done
			}
			return nil
		case socketErr := <-socketErrors:
			if cancel != nil {
				cancel()
				<-done
			}
			return socketErr
		case <-requests:
			pending = true
			if cancel != nil {
				cancel()
			} else {
				start()
			}
		case runErr := <-done:
			cancel = nil
			done = nil
			if errors.Is(runErr, state.ErrStale) {
				pending = true
			} else if runErr != nil && !errors.Is(runErr, context.Canceled) {
				reconciler.Logger.Printf("%v", runErr)
			}
			if pending {
				start()
			}
		}
	}
}

func Notify(store *state.Store) (err error) {
	socket := &net.UnixAddr{Name: store.SocketPath(), Net: "unixgram"}
	connection, err := net.DialUnix("unixgram", nil, socket)
	if err != nil {
		return fmt.Errorf("cannot reach refresh service: %w", err)
	}
	defer func() {
		if closeErr := connection.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("cannot close refresh connection: %w", closeErr))
		}
	}()
	if err := connection.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return fmt.Errorf("cannot set refresh request timeout: %w", err)
	}
	if _, err := connection.Write([]byte(refreshMessage)); err != nil {
		return fmt.Errorf("cannot request refresh: %w", err)
	}
	return nil
}
