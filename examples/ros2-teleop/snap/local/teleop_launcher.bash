#!/usr/bin/bash
set -euo pipefail

params_file="${SNAP_COMMON:?SNAP_COMMON must be set}/ros2-params/teleop.yaml"
if [[ ! -f "$params_file" || ! -r "$params_file" ]]; then
    printf '%s\n' \
        "Cannot read agent-provided parameters: $params_file" \
        "Connect my-ros2-teleop-test:ros2-params to ros2-confdb-agent:ros-params" \
        "and configure an agent source with output teleop.yaml before starting." >&2
    exit 1
fi

exec ros2 run key_teleop key_teleop --ros-args --params-file "$params_file" "$@"
