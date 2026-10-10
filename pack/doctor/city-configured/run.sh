#!/bin/sh
# doctor/city-configured: GONK_CITY is set. There is no default (OD-1): a
# wrong city name 404s every dispatch. Exit protocol: 0=OK, 1=Warning,
# 2=Error (pack-spec.md 1.2.10).
set -eu
if [ -z "${GONK_CITY:-}" ]; then
	echo "GONK_CITY is not set -- every dispatch will 404"
	exit 2
fi
echo "GONK_CITY=${GONK_CITY}"
