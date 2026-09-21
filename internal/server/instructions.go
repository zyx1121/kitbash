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
	"it carries a kitbash.yaml with a name, a description and its deploy units, " +
	"next to a Dockerfile. This is a whole Package, with a second unit " +
	"beside the first:\n\n" +
	ExampleManifest +
	ExampleContainerfile +
	"\nUnits of one Package reach each other on localhost, and exactly one " +
	"of them declares expose. " +
	"Folders inside a Package need no manifest of their own. Write all files " +
	"of a Package in one fs_write with files. A mount source is any folder a " +
	"manifest above it describes. Then pkg_build, proc_run, proc_logs, which " +
	"reads the exposed unit unless you name another. expose: mcp " +
	"puts the Process's declared tools on this surface, expose: http is served at " +
	"https://<name>.<member>.<domain> when the host has a domain, else a port on this host, expose: none is a job, and with schedule (five field cron, UTC) " +
	"kitbashd runs it on time. Secrets never go into Files: " +
	"secrets_set stores a value and the unit names it under secrets. tel_query " +
	"shows what ran and what it cost. When the task is to make something that " +
	"runs, make it here: write the files, build, run, read the logs."

// ExampleManifest and ExampleContainerfile are the Package the instructions
// carry, as literal text. They are constants of their own so a test can write
// the manifest into a folder and load it with the real loader: the paragraph
// teaches this file to every agent that connects, and a manifest the host
// would refuse is the most expensive thing it could teach.
//
// The example declares two units because that is the shape an agent gets wrong
// on its own: the M11 rounds folded a cache into the application's own
// container rather than declaring it, see PLAN.md section 5.6. One unit is
// built from this folder and is the face, the other is an upstream image
// pinned by digest, and the first reaches the second on localhost.
//
// The two lines beginning with a name are the file names, and what is indented
// under each is that file. <member> is the caller's own name, which is the one
// placeholder here.
const ExampleManifest = "kitbash.yaml\n" +
	"  name: app\n" +
	"  description: What this does, one sentence.\n" +
	"  deploy:\n" +
	"    units:\n" +
	"      - name: web\n" +
	"        type: container\n" +
	"        build: .\n" +
	"        expose: http      # or mcp, or none\n" +
	"        port: 8080\n" +
	"        environment: { CACHE: \"redis://localhost:6379\" }\n" +
	"        mounts:           # optional, for state that must survive a restart\n" +
	"          - { source: /home/<member>/app-data, target: /data, mode: rw }\n" +
	"      - name: cache\n" +
	"        type: container\n" +
	"        image: docker.io/library/redis@sha256:520775a41a63e77e06c73e35d2fd9cc15921a609516818796b4ecbb813078bc7\n"

// ExampleContainerfile is the other half: a Dockerfile beside the manifest,
// which is what build: . builds.
const ExampleContainerfile = "Dockerfile\n" +
	"  FROM node:22-alpine\n" +
	"  WORKDIR /app\n" +
	"  COPY . .\n" +
	"  RUN npm install --omit=dev\n" +
	"  CMD [\"node\", \"server.js\"]\n"
