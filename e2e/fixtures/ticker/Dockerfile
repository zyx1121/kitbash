# ticker: a container that runs and exits, which is what a scheduled unit is.
#
# The base is the digest the echo fixture pins, so this build is that image
# plus one copy and the end to end job pulls nothing more, see fixtures/echo.
FROM docker.io/library/node:22-alpine@sha256:c610fcdfb1d5b4740dd70c284ed3cb16bb857e0f7166196e36a5501df7a3aa32

WORKDIR /app

COPY server.js ./

ENTRYPOINT ["node", "/app/server.js"]
