# import-cli: a stdio MCP server. kitbash runs it as PID 1 with stdin held open
# and execs one more instance of this entrypoint per MCP session.
FROM node:22-alpine

WORKDIR /app

# No dependencies. The parser, the generator and the server are plain Node, so a
# build of this Package is the base image plus one copy, and nothing third party
# gets a say in what a manifest this kit writes contains.
#
# adapter.js is not used by the server. It is the file every Package this kit
# drafts carries, copied out verbatim as one of the files import answers with,
# so it has to be in the image beside the code that hands it out.
COPY package.json kitbash.yaml index.js parse.js generate.js adapter.js ./

ENTRYPOINT ["node", "/app/index.js"]
