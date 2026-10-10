# ADR-008 prototype agent image.
#
# THIS IS A SPIKE IMAGE, NOT images/Dockerfile.agent. It exists only to carry
# the compiled L1 synthetic scenario into a Job so the Job mechanics can be
# exercised end to end. The real change (T-55) runs the EXISTING agent image;
# nothing here is proposed for production, and images/versions.env is untouched.
#
# The L1 binary is built on the host with:
#   CGO_ENABLED=0 go test -c ./test/integration -o integration.test
# and is fully self-contained -- verified by running it from an empty cwd with
# no repo present.
FROM docker.io/library/alpine:3.20

RUN apk add --no-cache coreutils tar

COPY integration.test /usr/local/bin/integration.test
COPY entrypoint.sh    /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/integration.test /usr/local/bin/entrypoint.sh

# Non-root, matching the posture the chart gives agent pods.
RUN adduser -D -u 10001 agent
USER 10001

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
