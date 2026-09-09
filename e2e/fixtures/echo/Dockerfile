# echo: a stdio MCP server. kitbash runs it as PID 1 with stdin held open and
# execs one more instance of this entrypoint for every MCP session.
FROM docker.io/library/node:22-alpine

WORKDIR /app

# No dependencies: the server is one file of plain Node, so a build of this
# Package is the base image plus one copy and the end to end job stays inside
# its budget.
COPY server.js ./

ENTRYPOINT ["node", "/app/server.js"]
