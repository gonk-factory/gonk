#!/bin/sh
# Thin wrapper. ALL logic is in the Go binary, where it is tested.
# gonk-sweep is a cooldown exec order -- it takes no declared params.
set -eu
exec gonk-gate sweep
