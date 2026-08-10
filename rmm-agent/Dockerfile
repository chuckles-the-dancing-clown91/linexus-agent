# Linexus RMM agent. Note: an agent normally runs on the managed host itself,
# not in a container — this image is for demos and for hosts that run it in a
# privileged container with host mounts (hence no USER drop: managing the host
# is the job).
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/rmm-agent .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && mkdir -p /var/lib/linexus
COPY --from=build /out/rmm-agent /usr/local/bin/rmm-agent
ENV AGENT_STATE_FILE=/var/lib/linexus/agent-state.json
ENTRYPOINT ["rmm-agent"]
