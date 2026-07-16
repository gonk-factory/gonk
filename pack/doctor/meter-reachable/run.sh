#!/bin/sh
# doctor/meter-reachable: gonk-meter answers /healthz. Without it NOTHING may
# spend, and every dispatch fails closed. Exit protocol: 0=OK, 1=Warning,
# 2=Error (pack-spec.md 1.2.10 / internal/config/config.go's PackDoctorEntry).
#
# Assumes GONK_METER_URL is set in the controller process's own environment
# (the chart injects it there for gonk-gate's exec-order invocations); this
# check reads it the same way rather than guessing a default.
set -eu
if [ -z "${GONK_METER_URL:-}" ]; then
	echo "GONK_METER_URL is not set"
	exit 2
fi
if ! curl -fsS --max-time 5 "${GONK_METER_URL%/}/healthz" >/dev/null 2>&1; then
	echo "gonk-meter at ${GONK_METER_URL} did not answer /healthz"
	exit 2
fi
echo "gonk-meter is reachable"
