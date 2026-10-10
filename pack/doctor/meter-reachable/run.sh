#!/bin/sh
# doctor/meter-reachable: gonk-meter answers /healthz. Without it NOTHING may
# spend, and every dispatch fails closed. Exit protocol: 0=OK, 1=Warning,
# 2=Error (pack-spec.md 1.2.10 / internal/config/config.go's PackDoctorEntry).
#
# THIS CHECK DOES NOT USE curl, AND MUST NOT (gonk-hckm). It used to:
#
#     if ! curl -fsS --max-time 5 "${GONK_METER_URL%/}/healthz" >/dev/null 2>&1
#
# images/Dockerfile.controller keeps curl OUT of the runtime image on purpose
# ("A separate stage so curl/tar never ship in the runtime image"). So the shell
# returned 127, `if !` swallowed that as a failed request, and this check
# reported the meter as unreachable on every controller boot from 2026-09-08
# onward -- WITHOUT EVER DIALLING IT. The meter was healthy throughout. Adding
# curl to the runtime image would "fix" it by putting a shell HTTP client into
# production to satisfy a health check; the GET moved into gonk-gate instead,
# a binary this image already ships.
#
# EVERY BRANCH BELOW SAYS WHETHER THE METER WAS CONTACTED. That is the actual
# lesson: the doctor protocol has three exit codes and more than three things
# can go wrong, so a check that could not run has to say so in words or it is
# indistinguishable from a check that ran and failed.
#
# Assumes GONK_METER_URL is set in the controller process's own environment
# (the chart injects it there for gonk-gate's exec-order invocations); this
# check reads it the same way rather than guessing a default.
set -eu

# GONK_GATE_BIN exists so the check can be exercised against a stub; the
# controller never sets it and gets the gonk-gate on PATH.
GATE="${GONK_GATE_BIN:-gonk-gate}"

if ! command -v "${GATE}" >/dev/null 2>&1; then
	echo "CANNOT RUN: ${GATE} is not on PATH, so this check could not be performed. The meter was NOT contacted and this says NOTHING about its health (gonk-hckm)."
	exit 2
fi

if ! "${GATE}" capabilities 2>/dev/null | grep -qx http-probe; then
	echo "CANNOT RUN: ${GATE} does not list the http-probe capability, so this check could not be performed. The binary in this image predates gonk-hckm. The meter was NOT contacted and this says NOTHING about its health."
	exit 2
fi

if [ -z "${GONK_METER_URL:-}" ]; then
	echo "GONK_METER_URL is not set, so there is no meter address to contact. This is missing configuration, not an unhealthy meter."
	exit 2
fi

probe_rc=0
probe_out=$("${GATE}" http-probe --timeout 5s "${GONK_METER_URL%/}/healthz" 2>&1) || probe_rc=$?

case "${probe_rc}" in
0)
	echo "gonk-meter is reachable: ${probe_out}"
	;;
126 | 127)
	echo "CANNOT RUN: ${GATE} http-probe could not be executed (exit ${probe_rc}). The meter was NOT contacted and this says NOTHING about its health."
	exit 2
	;;
*)
	echo "gonk-meter at ${GONK_METER_URL} is not healthy: ${probe_out}"
	exit 2
	;;
esac
