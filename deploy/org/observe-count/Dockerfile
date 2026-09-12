# observe-count: an HTTP subscriber, not a stdio MCP server. kitbashd runs it as
# PID 1 and POSTs the Telemetry fan out to port 8080.
FROM node:22-alpine

WORKDIR /app

# No dependencies: the kit is one file of plain Node, so there is nothing to
# install and nothing to pin.
COPY kitbash.yaml index.js ./

ENTRYPOINT ["node", "/app/index.js"]
