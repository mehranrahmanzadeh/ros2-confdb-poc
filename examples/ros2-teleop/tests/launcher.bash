#!/usr/bin/bash
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
temporary=$(mktemp -d)
trap 'rm -rf -- "$temporary"' EXIT

export SNAP_COMMON="$temporary/common with spaces"
export TEST_ARGS="$temporary/arguments"
mkdir -p "$temporary/bin" "$SNAP_COMMON"
export PATH="$temporary/bin:$PATH"
cat > "$temporary/bin/ros2" <<'EOF'
#!/usr/bin/bash
printf '%s\n' "$@" > "$TEST_ARGS"
EOF
chmod +x "$temporary/bin/ros2"

expect_missing() {
    if bash "$root/snap/local/teleop_launcher.bash" > "$temporary/stdout" 2> "$temporary/stderr"; then
        printf 'Launcher accepted a missing parameter file.\n' >&2
        exit 1
    fi
    grep -q 'Cannot read agent-provided parameters' "$temporary/stderr"
    [[ ! -e "$TEST_ARGS" ]]
}

expect_missing
mkdir -p "$SNAP_COMMON/ros2-params"
expect_missing
mkdir "$SNAP_COMMON/ros2-params/teleop.yaml"
expect_missing
rmdir "$SNAP_COMMON/ros2-params/teleop.yaml"
cp "$root/config/teleop.yaml" "$SNAP_COMMON/ros2-params/teleop.yaml"

bash "$root/snap/local/teleop_launcher.bash" -r '__ns:=/demo namespace'
mapfile -t actual < "$TEST_ARGS"
expected=(run key_teleop key_teleop --ros-args --params-file
    "$SNAP_COMMON/ros2-params/teleop.yaml" -r '__ns:=/demo namespace')
[[ "${#actual[@]}" -eq "${#expected[@]}" ]]
for i in "${!expected[@]}"; do
    [[ "${actual[$i]}" == "${expected[$i]}" ]]
done
printf 'Launcher tests passed.\n'
