# agent-template: a stdio MCP server. kitbash runs it as PID 1 with stdin held
# open and execs one more instance of this entrypoint per MCP session.
FROM node:22-alpine

WORKDIR /app

# The direct versions are pinned in package.json. Transitive versions are not,
# so two builds of this image can differ; what a Process is pinned to is the
# image digest, not this file. --omit=dev leaves out the YAML reader, which only
# the tests need.
COPY package.json ./
RUN npm install --omit=dev --no-audit --no-fund

# AGENT.md is the prompt this kit declares and the loop reads it into every
# system prompt, so it has to be in the image. kitbash.yaml travels with it: the
# schemas the server publishes are constants in agent.js and a test holds them to
# the manifest, so the image carries the manifest those constants were checked
# against.
COPY kitbash.yaml AGENT.md agent.js index.js ./

ENTRYPOINT ["node", "/app/index.js"]
