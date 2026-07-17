#!/bin/sh
# Thin wrapper. ALL logic is in the Go binary, where it is tested.
# Gas City namespaces an order's declared [order.params] into the exec
# environment as GC_WEBHOOK_ARG_*; gonk-gate reads them from there.
set -eu
exec gonk-gate dispatch
