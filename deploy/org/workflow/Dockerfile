# workflow: a stdio MCP server. kitbash runs it as PID 1 with stdin held open
# and execs one more instance of this entrypoint per MCP session.
FROM node:22-alpine

WORKDIR /app

# The direct versions are pinned in package.json. Transitive versions are not,
# so two builds of this image can differ; what a Process is pinned to is the
# image digest, not this file.
COPY package.json ./
RUN npm install --omit=dev --no-audit --no-fund

# kitbash.yaml is the source of the tool schemas the server advertises, and
# WORKFLOW.md is the prompt it declares, so the image carries both next to the
# code.
COPY kitbash.yaml WORKFLOW.md index.js ./

ENTRYPOINT ["node", "/app/index.js"]
