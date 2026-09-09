# echo: a stdio MCP server. kitbash runs it as PID 1 with stdin held open and
# execs one more instance of this entrypoint for every MCP session.
#
# The base is pinned by digest. A tag is a moving target, and a job that fails
# because the registry moved node:22-alpine under it is a job nobody trusts;
# the workflow pulls this digest before the surface test so the build itself
# never waits on the network.
FROM docker.io/library/node:22-alpine@sha256:c610fcdfb1d5b4740dd70c284ed3cb16bb857e0f7166196e36a5501df7a3aa32

WORKDIR /app

# No dependencies: the server is one file of plain Node, so a build of this
# Package is the base image plus one copy and the end to end job stays inside
# its budget.
COPY server.js ./

ENTRYPOINT ["node", "/app/server.js"]
