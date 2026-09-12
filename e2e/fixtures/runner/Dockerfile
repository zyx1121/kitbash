# runner: a stdio MCP server that implements the run hook. kitbash runs it as
# PID 1 with stdin held open and execs one more instance of this entrypoint for
# every MCP session, the same way it runs any Package with expose: mcp.
#
# The base is the one the echo fixture pins, so the end to end job pulls one
# image and builds two Packages on top of it and stays inside its budget.
FROM docker.io/library/node:22-alpine@sha256:c610fcdfb1d5b4740dd70c284ed3cb16bb857e0f7166196e36a5501df7a3aa32

WORKDIR /app

COPY server.js ./

ENTRYPOINT ["node", "/app/server.js"]
