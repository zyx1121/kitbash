package server

// Instructions is what `initialize` answers with, the one field of the
// protocol that lands in a client's system prompt without anyone asking for
// it, see PLAN.md section 4.5. The agents that connect are general ones, and a
// general agent asked for a web application scaffolds it on the machine it
// runs on unless something tells it otherwise; this text is what tells it
// otherwise, and it is the whole deployment conversation.
//
// It lives alone in this file because it is edited as prose and measured as a
// cost: every turn of every session carries it, so it stays one paragraph and
// under 1200 bytes, see instructions_test.go. A Process's own session over
// /mcp is given the same text, because it reaches the same surface and there
// is nothing to special case.
const Instructions = "This host runs kitbash, and it is where the work should run and live " +
	"rather than the machine you are running on. Files are the source: /org is " +
	"shared, /home/<member> is the caller's own, fs_read and fs_write read and " +
	"write them, and every write is a commit. A folder becomes a Package when " +
	"it carries a kitbash.yaml with a name, a description and one deploy unit " +
	"(build: . and an expose), next to a Dockerfile; that is all a Package " +
	"needs. pkg_build builds it and proc_run runs it as a container under the " +
	"caller. expose: mcp puts the Process's declared tools on this surface, " +
	"expose: http publishes a port on this host, expose: none is a job. Secrets " +
	"never go into Files: secrets_set stores a value and the unit names it " +
	"under secrets. proc_logs and tel_query show what ran and what it cost. " +
	"Nothing else about deployment needs to be decided or discussed. When the " +
	"task is to make something that runs, make it here: write the files, build, " +
	"run, read the logs."
