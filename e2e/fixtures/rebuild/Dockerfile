# rebuild: an image that exists to be built twice in quick succession.
#
# The base is the one the echo fixture pins, so the workflow's pull warms this
# build too and neither of the two builds waits on the network. The only layer
# of its own is one small file, which is what makes a second build of this
# Package finish inside the same second as the first.
FROM docker.io/library/node:22-alpine@sha256:c610fcdfb1d5b4740dd70c284ed3cb16bb857e0f7166196e36a5501df7a3aa32

WORKDIR /app

COPY marker.txt ./

# PID 1 holds stdin open and does nothing else, which is all this Process has
# to do to be running when proc_run answers.
ENTRYPOINT ["node", "-e", "process.stdin.resume()"]
