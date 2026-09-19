package server

// Instructions is what `initialize` answers with, the one field of the
// protocol that lands in a client's system prompt without anyone asking for
// it, see PLAN.md section 4.5. The agents that connect are general ones, and a
// general agent asked for a web application scaffolds it on the machine it
// runs on unless something tells it otherwise; this text is what tells it
// otherwise, and it is the whole deployment conversation.
//
// It lives alone in this file because it is edited as prose and measured as a
// cost: every turn of every session carries it, so it stays under 2 KiB, see
// instructions_test.go. What it costs is measured against what it saves. The
// first clean agent trial paid eleven calls to learn the shape of a manifest,
// four to learn that a folder inside a Package needed one of its own, and
// thirteen to write one file at a time, so the text carries a whole minimal
// Package and says both of those things outright.
//
// The example is real: a test writes it into a folder and loads it with the
// manifest loader, so a manifest that stopped validating cannot stay in the
// paragraph that teaches it. A Process's own session over /mcp is given the
// same text, because it reaches the same surface and there is nothing to
// special case.
const Instructions = "This host runs kitbash, and it is where the work should run and live " +
	"rather than the machine you are running on. Files are the source: /org is " +
	"shared, /home/<member> is the caller's own, fs_read and fs_write read and " +
	"write them, and every write is a commit. A folder becomes a Package when " +
	"it carries a kitbash.yaml with a name, a description and one deploy unit, " +
	"next to a Dockerfile. This is the whole of a minimal Package:\n\n" +
	ExampleManifest +
	ExampleContainerfile +
	"\nFolders inside a Package need no manifest of their own. Write all files " +
	"of a Package in one fs_write with files. A mounted source folder needs " +
	"its own kitbash.yaml. Then pkg_build, proc_run, proc_logs. expose: mcp " +
	"puts the Process's declared tools on this surface, expose: http publishes " +
	"a port on this host, expose: none is a job. Secrets never go into Files: " +
	"secrets_set stores a value and the unit names it under secrets. tel_query " +
	"shows what ran and what it cost. When the task is to make something that " +
	"runs, make it here: write the files, build, run, read the logs."

// ExampleManifest and ExampleContainerfile are the Package the instructions
// carry, as literal text. They are constants of their own so a test can write
// the manifest into a folder and load it with the real loader: the paragraph
// teaches this file to every agent that connects, and a manifest the host
// would refuse is the most expensive thing it could teach.
//
// The two lines beginning with a name are the file names, and what is indented
// under each is that file. <member> is the caller's own name, which is the one
// placeholder here.
const ExampleManifest = "kitbash.yaml\n" +
	"  name: app\n" +
	"  description: What this does, one sentence.\n" +
	"  deploy:\n" +
	"    units:\n" +
	"      - type: container\n" +
	"        build: .\n" +
	"        expose: http        # or mcp, or none\n" +
	"        mounts:             # optional, for state that must survive a restart\n" +
	"          - { source: /home/<member>/app-data, target: /data, mode: rw }\n"

// ExampleContainerfile is the other half: a Dockerfile beside the manifest,
// which is what build: . builds.
const ExampleContainerfile = "Dockerfile\n" +
	"  FROM node:22-alpine\n" +
	"  WORKDIR /app\n" +
	"  COPY . .\n" +
	"  RUN npm install --omit=dev\n" +
	"  CMD [\"node\", \"server.js\"]\n"
