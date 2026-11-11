package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
)

type SnapctlView struct {
	Plug string
	run  func(context.Context, ...string) ([]byte, error)
}

func (view SnapctlView) Read(ctx context.Context, previous bool) ([]byte, error) {
	run := view.run
	if run == nil {
		run = func(ctx context.Context, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, "snapctl", args...).Output()
		}
	}
	fields := []struct {
		name         string
		defaultValue string
	}{
		{name: "sources", defaultValue: "[]"},
		{name: "policy", defaultValue: "{}"},
		{name: "credentials", defaultValue: "[]"},
	}
	result := make(map[string]json.RawMessage, len(fields))
	for _, field := range fields {
		args := []string{"get", "--view"}
		if previous {
			args = append(args, "--previous")
		}
		args = append(args, "--default", field.defaultValue, "-d", ":"+view.Plug, field.name)
		data, err := run(ctx, args...)
		if err != nil {
			return nil, fmt.Errorf("snapctl could not read confdb field %s: %w", field.name, err)
		}
		var document map[string]json.RawMessage
		if err := json.Unmarshal(data, &document); err != nil || len(document) != 1 {
			return nil, fmt.Errorf("snapctl returned an invalid document for confdb field %s", field.name)
		}
		value, ok := document[field.name]
		if !ok {
			return nil, fmt.Errorf("snapctl omitted confdb field %s", field.name)
		}
		result[field.name] = value
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("cannot assemble confdb view: %w", err)
	}
	return data, nil
}

func FailHook(ctx context.Context, message string) error {
	if err := exec.CommandContext(ctx, "snapctl", "fail", message).Run(); err != nil {
		return fmt.Errorf("snapctl could not reject the confdb change: %w", err)
	}
	return nil
}
