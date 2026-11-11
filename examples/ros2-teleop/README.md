# ROS 2 teleop consumer example

Adapted from [canonical/snap_configuration](https://github.com/canonical/snap_configuration/tree/e489c46d684f401cfcccd32a7b35dfe7405b976c).

The only configuration source is the agent's read-only content slot. There is
no polling, bundled fallback, or direct confdb access in this snap. The
[sample parameter file](config/teleop.yaml) is for serving to the agent; it is
not packaged into the consumer.

## 1. Build and install

From this directory:

```sh
snapcraft pack --use-lxd
sudo snap install --dangerous ./my-ros2-teleop-test_0.1_amd64.snap
sudo snap connect my-ros2-teleop-test:ros2-params ros2-confdb-agent:ros-params
snap connections my-ros2-teleop-test
```

If an older upstream example is installed under this name, use
`sudo snap refresh --dangerous ./my-ros2-teleop-test_0.1_amd64.snap` instead of
the install command.
The agent must already be installed with its three confdb plugs connected.

## 2. Configure the agent

Use the account ID that owns your installed agent's confdb schema:

```sh
ACCOUNT_ID='<your-confdb-owner-account-id>'
sudo snap set -t "$ACCOUNT_ID/ros2/sources" \
  'sources=[{"source":"http://example.com/teleop.yaml","output":"teleop.yaml","version":1}]'
sudo snap logs -f ros2-confdb-agent.daemon
```

This replaces the entire source list; preserve any existing sources you still
need, and give every source a unique output filename.

Wait for `published output "teleop.yaml"` in the agent log, then stop following
the log with Ctrl+C. A committed source entry does not mean its asynchronous
download has finished.

## 3. Run and observe

Run in an interactive terminal:

```sh
snap run my-ros2-teleop-test
```

The launcher passes the mounted file to:

```text
ros2 run key_teleop key_teleop --ros-args --params-file <mounted teleop.yaml>
```

Arrow keys display linear and angular velocities using the rates from the
sample configuration; press `q` to exit.

The launcher exits with an explicit error if the content connection or file is
missing. File contents are validated by ROS, not by the agent.

## 4. Change configuration

Exit the application. Edit the served [teleop.yaml](config/teleop.yaml), for
example changing `forward_rate` from `0.2` to `0.4`. Keep the integer/float
types as shown. Then request a download:

```sh
sudo snap set -t "$ACCOUNT_ID/ros2/sources" \
  'sources=[{"source":"http://example.com/teleop.yaml","output":"teleop.yaml","version":2}]'
```

Wait for the publication log, then run `snap run my-ros2-teleop-test` again.
