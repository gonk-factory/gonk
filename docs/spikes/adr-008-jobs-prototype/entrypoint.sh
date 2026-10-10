#!/bin/sh
# ADR-008 prototype agent entrypoint.
#
# It stands in for the real agent image for ONE purpose: to prove that a
# Kubernetes Job can carry everything a Gas City session carries, and that
# gonk-gate can read completion and the proposed-effects batch back out
# WITHOUT an HTTP client, an event stream, or a tmux pane.
#
# Everything this script consumes arrives the way ADR-008 says it should:
# as files in Secret volumes. Nothing is injected as keystrokes, and nothing
# is fetched from a controller.
set -eu

PROMPT_FILE=/var/run/gonk/prompt/prompt.txt
KEY_FILE=/var/run/gonk/key/virtual-key
CHECKOUT_FILE=/var/run/gonk/checkout/checkout.tar.gz
WORKDIR=/work

echo "gonk-l1-agent: starting"

# 1. The prompt. In Gas City this is the step that fails 3 runs in 4
#    (gonk-njz): it is typed into an opencode TUI pane and may simply never
#    land. Here it is a file, and its absence is a hard error, not a pod that
#    sits at a splash screen for its whole life.
if [ ! -r "$PROMPT_FILE" ]; then
	echo "gonk-l1-agent: FATAL: prompt not mounted at $PROMPT_FILE" >&2
	exit 64
fi
echo "gonk-l1-agent: prompt mounted, $(wc -c < "$PROMPT_FILE") bytes"

# 2. The virtual key. Read, length-reported, NEVER echoed.
if [ ! -r "$KEY_FILE" ]; then
	echo "gonk-l1-agent: FATAL: virtual key not mounted at $KEY_FILE" >&2
	exit 65
fi
echo "gonk-l1-agent: virtual key mounted, $(wc -c < "$KEY_FILE") bytes (value not logged)"

# 3. The checkout tarball.
if [ ! -r "$CHECKOUT_FILE" ]; then
	echo "gonk-l1-agent: FATAL: checkout tarball not mounted at $CHECKOUT_FILE" >&2
	exit 66
fi
# --no-same-owner/--no-same-permissions: the agent runs as an unprivileged UID
# and must not try to restore the tarball's ownership or mode bits onto the
# emptyDir. Without them tar fails on the archive root with "Operation not
# permitted" -- observed on the first prototype run.
mkdir -p "$WORKDIR/src"
tar -xzf "$CHECKOUT_FILE" -C "$WORKDIR/src" --no-same-owner --no-same-permissions
echo "gonk-l1-agent: checkout extracted, $(find "$WORKDIR/src" -type f | wc -l) files"

# 4. Run the REAL L1 synthetic scenario. This is the repo's own
#    test/integration suite, compiled with `go test -c`, which drives
#    test/harness.SyntheticSession through /decide -> LLM calls -> /outcome
#    against the real intake and meter packages. It is the same code the
#    standing gate runs; it is not a reimplementation.
set +e
/usr/local/bin/integration.test -test.count=1 -test.v=false > /tmp/l1.out 2>&1
L1_RC=$?
set -e
cat /tmp/l1.out
echo "gonk-l1-agent: L1 exit=$L1_RC"

# 5. Emit the proposed-effects batch, fenced with the SAME sentinels
#    cmd/gonk-gate/broker_apply.go already looks for. This is the read path
#    ADR-008 replaces GetSessionOutput/GetSessionTranscript with: the batch is
#    on stdout, so it is in the pod log, so it survives the pod.
if [ "$L1_RC" -eq 0 ]; then
	OUTCOME=complete
else
	OUTCOME=failed
fi
echo "GONK_BATCH_START"
printf '{"scenario":"L1-synthetic","outcome":"%s","l1_exit_code":%d,"prompt_sha256":"%s"}\n' \
	"$OUTCOME" "$L1_RC" "$(sha256sum "$PROMPT_FILE" | cut -d' ' -f1)"
echo "GONK_BATCH_END"

echo "gonk-l1-agent: done"
exit "$L1_RC"
