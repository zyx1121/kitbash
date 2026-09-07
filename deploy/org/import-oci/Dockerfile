# import-oci: a stdio MCP server. kitbash runs it as PID 1 with stdin held open
# and execs one more instance of this entrypoint per MCP session.
FROM node:22-alpine

WORKDIR /app

# skopeo reads an image manifest straight out of a registry, so the kit never
# pulls a layer and never needs a container runtime of its own.
RUN apk add --no-cache skopeo

# Versions are pinned exactly in package.json, so no lock file is needed and
# npm resolves the same tree every build.
COPY package.json ./
RUN npm install --omit=dev --no-audit --no-fund

# kitbash.yaml is the source of the tool schemas the server advertises, so the
# image carries it next to the code.
COPY kitbash.yaml index.js ./

ENTRYPOINT ["node", "/app/index.js"]
