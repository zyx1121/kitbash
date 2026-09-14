# reader: a stdio MCP server that reads and writes files through a bind mount.
#
# The base is the one the echo fixture pins, so the end to end job pulls one
# image for both and this build is that image plus one copy.
FROM docker.io/library/node:22-alpine@sha256:c610fcdfb1d5b4740dd70c284ed3cb16bb857e0f7166196e36a5501df7a3aa32

WORKDIR /app

COPY server.js ./

ENTRYPOINT ["node", "/app/server.js"]
