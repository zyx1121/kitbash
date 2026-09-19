package server

import "encoding/json"

// The schemas below are the fs family of spec/mcp-surface.yaml, transcribed to
// JSON Schema 2020-12. They are declared here rather than inferred from Go
// types so the wire contract stays the one the spec publishes.

const folderEntryDef = `{
  "type": "object",
  "required": ["path", "name"],
  "properties": {
    "path": { "type": "string" },
    "name": { "type": "string" },
    "description": { "type": "string", "description": "From the folder's own kitbash.yaml; absent for a folder inside a Package, which the Package describes" }
  }
}`

const fileEntryDef = `{
  "type": "object",
  "required": ["name", "size"],
  "properties": {
    "name": { "type": "string" },
    "size": { "type": "integer" }
  }
}`

const writeFileDef = `{
  "description": "One file of an fs_write that carries a list. Exactly one of content and contentBase64 is set.",
  "type": "object",
  "additionalProperties": false,
  "required": ["path"],
  "oneOf": [
    { "required": ["content"] },
    { "required": ["contentBase64"] }
  ],
  "properties": {
    "path": { "type": "string", "description": "Absolute path, under the same top level folder as every other file of this call" },
    "content": { "type": "string", "description": "UTF-8 text" },
    "contentBase64": { "type": "string", "contentEncoding": "base64" }
  }
}`

const commitDef = `{
  "type": "object",
  "required": ["sha", "author", "time", "message"],
  "properties": {
    "sha": { "type": "string", "pattern": "^[a-f0-9]{40}$" },
    "author": { "type": "string", "description": "Linux user name" },
    "time": { "type": "string", "format": "date-time" },
    "message": { "type": "string" }
  }
}`

var (
	listInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "path": { "type": "string", "description": "Absolute folder path. Omit for the roots." }
  }
}`)

	listOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["path", "folders"],
  "properties": {
    "path": { "type": "string" },
    "manifest": { "type": "object", "description": "The kitbash.yaml this folder carries itself, absent at the roots and inside a Package" },
    "folders": { "type": "array", "items": ` + folderEntryDef + ` },
    "files": { "type": "array", "description": "The files directly in this folder, absent at the roots and for a folder that holds none", "items": ` + fileEntryDef + ` }
  }
}`)

	readInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["path"],
  "properties": {
    "path": { "type": "string" },
    "offset": { "type": "integer", "minimum": 0, "description": "Byte offset for text files" },
    "limit": { "type": "integer", "minimum": 1, "maximum": 1048576, "description": "Max bytes for text files" }
  }
}`)

	// fs_read declares no output schema. Its result is content blocks only, so
	// a client that prefers structured content still shows the caller the file
	// body. The metadata shape it returns as its trailing text block is
	// fs.ReadMeta, and readMetaSchema below is that shape: it is not published
	// as the tool's output schema, and it is declared here so the block a
	// caller parses is held to spec/mcp-surface.yaml like every other answer.

	writeInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["message"],
  "oneOf": [
    {
      "required": ["path"],
      "not": { "required": ["files"] },
      "oneOf": [
        { "required": ["content"] },
        { "required": ["contentBase64"] }
      ]
    },
    {
      "required": ["files"],
      "not": { "anyOf": [{ "required": ["path"] }, { "required": ["content"] }, { "required": ["contentBase64"] }] }
    }
  ],
  "properties": {
    "path": { "type": "string" },
    "content": { "type": "string", "description": "UTF-8 text" },
    "contentBase64": { "type": "string", "contentEncoding": "base64" },
    "files": {
      "type": "array",
      "minItems": 1,
      "maxItems": 64,
      "description": "Several files as one commit, all under one top level folder, 16 MiB in total",
      "items": ` + writeFileDef + `
    },
    "message": { "type": "string", "minLength": 3, "maxLength": 200, "description": "Commit message, imperative mood" },
    "expectedSha": { "type": "string", "description": "Last known commit sha; optimistic lock, read against the file for one path and against the repository head for a list" }
  }
}`)

	writeOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["commit"],
  "oneOf": [
    { "required": ["path"] },
    { "required": ["paths"] }
  ],
  "properties": {
    "path": { "type": "string", "description": "The file that was written, for the one file form" },
    "paths": { "type": "array", "items": { "type": "string" }, "description": "The files that were written, for a list" },
    "commit": ` + commitDef + `
  }
}`)

	readMetaSchema = json.RawMessage(`{
  "description": "Content blocks only. fs_read declares no outputSchema and returns no structuredContent, because clients prefer structuredContent when it is present and would then show the caller the metadata instead of the file body. The first block is the file: text media types as a text block, image/png and image/jpeg as an image block. The trailing block is a text block holding this metadata object as JSON.",
  "type": "object",
  "required": ["path", "mediaType", "size"],
  "properties": {
    "path": { "type": "string" },
    "mediaType": { "type": "string" },
    "size": { "type": "integer" },
    "sha": { "type": "string", "description": "Commit that last touched this file" },
    "truncated": { "type": "boolean" }
  }
}`)

	historyInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["path"],
  "properties": {
    "path": { "type": "string" },
    "limit": { "type": "integer", "minimum": 1, "maximum": 100, "default": 20 }
  }
}`)

	historyOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["path", "commits"],
  "properties": {
    "path": { "type": "string" },
    "commits": { "type": "array", "items": ` + commitDef + ` }
  }
}`)
)

// The schemas below are the pkg and proc families of spec/mcp-surface.yaml,
// transcribed the same way.

const buildEntryDef = `{
  "type": "object",
  "required": ["digest", "commit", "builtAt"],
  "properties": {
    "digest": { "type": "string" },
    "commit": { "type": "string" },
    "builtAt": { "type": "string", "format": "date-time" },
    "builder": { "type": "string", "description": "The member who built it, for a Package under /org" }
  }
}`

// processDef is one Process as proc_list answers about it, and it is one
// shape rather than two: without a package the fields a line does not carry
// are simply absent, so a listing of either kind is read against this. A
// oneOf of two shapes would be a schema a full Process matches twice, see
// spec/mcp-surface.yaml.
const processDef = `{
  "type": "object",
  "required": ["id", "name", "package", "state"],
  "properties": {
    "id": { "type": "string" },
    "name": { "type": "string" },
    "package": { "type": "string" },
    "digest": { "type": "string", "description": "Absent from a line" },
    "state": { "type": "string", "enum": ["starting", "running", "unhealthy", "stopped", "failed", "scheduled"] },
    "expose": { "type": "string", "enum": ["mcp", "http", "none"] },
    "url": { "type": "string", "description": "Where an http Process is served, when the host has a domain" },
    "schedule": { "type": "string", "description": "The cron expression this Process is a job of, absent for a Process that stays up" },
    "nextRun": { "type": "string", "format": "date-time", "description": "When kitbashd runs this job next, UTC" },
    "lastRun": {
      "type": "object",
      "description": "The run this job finished most recently. Absent until it has run once under the daemon that is answering, which holds the runs it saw the way it holds health readings.",
      "required": ["startedAt", "exitCode", "durationMs"],
      "properties": {
        "startedAt": { "type": "string", "format": "date-time" },
        "exitCode": { "type": "integer" },
        "durationMs": { "type": "integer" }
      }
    },
    "startedAt": { "type": "string", "format": "date-time" },
    "problem": { "type": "string", "description": "Why this Process is not running, when kitbashd could not bring it back" },
    "fix": { "type": "string", "description": "What the owner can do about it" },
    "runner": { "type": "string", "description": "Package path of the run kit that owns this Process, absent when kitbashd runs it" },
    "mounts": {
      "type": "array",
      "description": "The folders of Files this Process sees, as the registration kitbashd holds records them. Absent for a Process that declared none.",
      "items": {
        "type": "object",
        "required": ["source", "target", "mode"],
        "properties": {
          "source": { "type": "string" },
          "target": { "type": "string" },
          "mode": { "type": "string", "enum": ["ro", "rw"] }
        }
      }
    },
    "health": {
      "type": "object",
      "description": "The most recent health probe kitbashd ran, for a Process kitbashd runs itself whose manifest declares deploy.units[0].health.http. Absent until it has been probed once.",
      "required": ["healthy"],
      "properties": {
        "last": { "type": "string", "format": "date-time" },
        "healthy": { "type": "boolean" }
      }
    }
  }
}`

var (
	pkgBuildInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["path"],
  "properties": {
    "path": { "type": "string" }
  }
}`)

	pkgBuildOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["path", "digest", "commit"],
  "properties": {
    "path": { "type": "string" },
    "digest": { "type": "string", "pattern": "^sha256:[a-f0-9]{64}$" },
    "commit": { "type": "string", "pattern": "^[a-f0-9]{40}$" },
    "log": { "type": "string", "description": "Tail of the build log" }
  }
}`)

	pkgImportInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["path", "source"],
  "properties": {
    "path": { "type": "string", "description": "New folder to create" },
    "source": { "type": "string", "description": "npm:<pkg>@<ver> today; oci://<ref>@sha256:... and pypi:<pkg>==<ver> once their kits exist" }
  }
}`)

	pkgImportOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["path", "commit"],
  "properties": {
    "path": { "type": "string" },
    "commit": ` + commitDef + `
  }
}`)

	pkgListInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {}
}`)

	pkgListOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["packages"],
  "properties": {
    "packages": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["path", "name"],
        "properties": {
          "path": { "type": "string" },
          "name": { "type": "string" },
          "digest": { "type": "string", "description": "The latest build, absent for a Package nothing has built" }
        }
      }
    }
  }
}`)

	pkgInspectInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["path"],
  "properties": {
    "path": { "type": "string" }
  }
}`)

	pkgInspectOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["path", "manifest", "builds"],
  "properties": {
    "path": { "type": "string" },
    "manifest": { "type": "object" },
    "builds": { "type": "array", "items": ` + buildEntryDef + ` }
  }
}`)

	procRunInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["package"],
  "properties": {
    "package": { "type": "string", "description": "Package path" },
    "digest": { "type": "string", "pattern": "^sha256:[a-f0-9]{64}$", "description": "Defaults to the latest build, see the description for how latest is decided" },
    "name": { "type": "string", "pattern": "^[a-z0-9]+(-[a-z0-9]+)*$", "maxLength": 64, "description": "Defaults to the package name" }
  }
}`)

	procRunOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["id", "name", "package", "digest", "state"],
  "properties": {
    "id": { "type": "string", "description": "UUIDv7" },
    "name": { "type": "string" },
    "package": { "type": "string" },
    "digest": { "type": "string" },
    "state": { "type": "string", "enum": ["starting", "running", "unhealthy", "stopped", "failed", "scheduled"] },
    "expose": { "type": "string", "enum": ["mcp", "http", "none"] },
    "endpoint": { "type": "string", "description": "Internal URL when expose is http" },
    "schedule": { "type": "string", "description": "The cron expression this Process is a job of, absent for a Process that stays up" },
    "nextRun": { "type": "string", "format": "date-time", "description": "When kitbashd runs this job next, UTC" },
    "lastRun": {
      "type": "object",
      "description": "The run this job finished most recently. Absent until it has run once under the daemon that is answering, which holds the runs it saw the way it holds health readings.",
      "required": ["startedAt", "exitCode", "durationMs"],
      "properties": {
        "startedAt": { "type": "string", "format": "date-time" },
        "exitCode": { "type": "integer" },
        "durationMs": { "type": "integer" }
      }
    },
    "tools": { "type": "array", "items": { "type": "string" }, "description": "Surface tool names added when expose is mcp" },
    "runner": { "type": "string", "description": "Package path of the run kit that owns this Process, absent when kitbashd runs it" }
  }
}`)

	procListInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "package": { "type": "string", "description": "Package path. Omit for one line per Process." }
  }
}`)

	procListOutputSchema = json.RawMessage(`{
  "type": "object",
  "description": "One line per Process without a package, and the full shape below with one.",
  "required": ["processes"],
  "properties": {
    "processes": { "type": "array", "items": ` + processDef + ` }
  }
}`)

	procStopInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["id"],
  "properties": {
    "id": { "type": "string" }
  }
}`)

	procStopOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["id", "state"],
  "properties": {
    "id": { "type": "string" },
    "state": { "type": "string" }
  }
}`)

	procLogsInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["id"],
  "properties": {
    "id": { "type": "string" },
    "lines": { "type": "integer", "minimum": 1, "default": 200, "maximum": 5000 }
  }
}`)

	procLogsOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["id", "lines"],
  "properties": {
    "id": { "type": "string" },
    "lines": { "type": "array", "items": { "type": "string" } }
  }
}`)
)

// The schemas below are the tel family of spec/mcp-surface.yaml, transcribed
// the same way. The three record shapes are the $defs the query output
// composes with oneOf, inlined here the way commitDef is.

const telAttributesDef = `{
  "type": "object",
  "description": "The four attributes plus tool, eval and internal, in short form. Absent when the record did not carry them.",
  "properties": {
    "user": { "type": "string" },
    "package": { "type": "string" },
    "process": { "type": "string" },
    "path": { "type": "string" },
    "tool": { "type": "string" },
    "eval": { "type": "boolean" },
    "producer": { "type": "string", "description": "Member name or Process id that wrote the record" },
    "caller": { "type": "string", "description": "Process id whose MCP session recorded this span, when a Process acted for its owner" },
    "internal": { "type": "boolean", "description": "true when the record is the cause of an internal problem; admins alone are answered these" },
    "subject": { "type": "object", "description": "For evaluation records: traceId and spanId of the judged span", "properties": { "traceId": { "type": "string" }, "spanId": { "type": "string" } } }
  }
}`

const spanRecordDef = `{
  "type": "object",
  "required": ["traceId", "spanId", "name", "start", "end", "status", "attributes"],
  "properties": {
    "traceId": { "type": "string", "pattern": "^[a-f0-9]{32}$" },
    "spanId": { "type": "string", "pattern": "^[a-f0-9]{16}$" },
    "parentSpanId": { "type": "string", "pattern": "^[a-f0-9]{16}$" },
    "name": { "type": "string", "description": "Tool name for surface spans, build or run for children" },
    "start": { "type": "string", "format": "date-time" },
    "end": { "type": "string", "format": "date-time" },
    "durationMs": { "type": "number" },
    "status": { "type": "string", "enum": ["ok", "error", "unset"] },
    "statusMessage": { "type": "string" },
    "attributes": ` + telAttributesDef + `,
    "other": { "type": "object", "description": "Remaining span attributes by their wire name" }
  }
}`

const logRecordDef = `{
  "type": "object",
  "required": ["time", "severity", "body", "attributes"],
  "properties": {
    "time": { "type": "string", "format": "date-time" },
    "severity": { "type": "string" },
    "body": { "type": "string" },
    "traceId": { "type": "string" },
    "spanId": { "type": "string" },
    "attributes": ` + telAttributesDef + `,
    "other": { "type": "object" }
  }
}`

const metricRecordDef = `{
  "type": "object",
  "required": ["time", "name", "value", "attributes"],
  "properties": {
    "time": { "type": "string", "format": "date-time" },
    "name": { "type": "string" },
    "value": { "type": "number" },
    "unit": { "type": "string" },
    "attributes": ` + telAttributesDef + `,
    "other": { "type": "object" }
  }
}`

var (
	telQueryInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["signal"],
  "properties": {
    "signal": { "type": "string", "enum": ["traces", "metrics", "logs"] },
    "user": { "type": "string", "description": "Linux user name; admins only when it is not the caller" },
    "package": { "type": "string", "description": "Package path, as kitbash.package" },
    "process": { "type": "string", "description": "Process id, as kitbash.process" },
    "path": { "type": "string", "description": "Files path, as kitbash.path; prefix match" },
    "tool": { "type": "string", "description": "Surface tool name, as kitbash.tool" },
    "eval": { "type": "boolean", "description": "Only evaluation results written back by kits" },
    "producer": { "type": "string", "description": "Member name or Process id that wrote the record" },
    "caller": { "type": "string", "description": "Process id whose MCP session recorded the span, as kitbash.caller" },
    "internal": { "type": "boolean", "description": "Only the causes of internal problems, which admins alone read; a member's query never returns them" },
    "since": { "type": "string", "format": "date-time" },
    "until": { "type": "string", "format": "date-time" },
    "limit": { "type": "integer", "minimum": 1, "default": 100, "maximum": 1000 }
  }
}`)

	telQueryOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["signal", "records", "truncated"],
  "properties": {
    "signal": { "type": "string" },
    "truncated": { "type": "boolean", "description": "true when more records matched than limit" },
    "records": {
      "type": "array",
      "items": {
        "oneOf": [
          ` + spanRecordDef + `,
          ` + logRecordDef + `,
          ` + metricRecordDef + `
        ]
      }
    }
  }
}`)

	telRetentionInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "set": {
      "type": "object",
      "additionalProperties": false,
      "properties": {
        "traces": { "type": "string", "pattern": "^[0-9]+(h|d)$" },
        "metrics": { "type": "string", "pattern": "^[0-9]+(h|d)$" },
        "logs": { "type": "string", "pattern": "^[0-9]+(h|d)$" }
      }
    }
  }
}`)

	telRetentionOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["traces", "metrics", "logs"],
  "properties": {
    "traces": { "type": "string" },
    "metrics": { "type": "string" },
    "logs": { "type": "string" }
  }
}`)
)

// The schemas below are the users and approvals families of
// spec/mcp-surface.yaml, transcribed the same way. Both families are served by
// kitbashd over its socket, so these are the only place kitbash-mcp says what
// their shape is, and the diff test holds them to the specification.

const memberDef = `{
  "type": "object",
  "required": ["user", "uid", "admin"],
  "properties": {
    "user": { "type": "string" },
    "uid": { "type": "integer" },
    "admin": { "type": "boolean" },
    "keys": { "type": "integer" },
    "processes": { "type": "integer" }
  }
}`

const approvalDef = `{
  "type": "object",
  "required": ["id", "requester", "tool", "input", "state", "requestedAt"],
  "properties": {
    "id": { "type": "string" },
    "requester": { "type": "string" },
    "tool": { "type": "string", "enum": ["fs_write", "pkg_import"] },
    "input": { "type": "object" },
    "state": { "type": "string", "enum": ["pending", "approved", "rejected"] },
    "requestedAt": { "type": "string", "format": "date-time" },
    "decidedAt": { "type": "string", "format": "date-time" },
    "decidedBy": { "type": "string" },
    "note": { "type": "string" },
    "reason": { "type": "string" },
    "result": { "type": "object" }
  }
}`

var (
	usersMeInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {}
}`)

	usersMeOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["user", "uid", "admin"],
  "properties": {
    "user": { "type": "string" },
    "uid": { "type": "integer" },
    "admin": { "type": "boolean" },
    "groups": { "type": "array", "items": { "type": "string" } }
  }
}`)

	usersCreateInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "sshKey"],
  "properties": {
    "name": { "type": "string", "pattern": "^[a-z][a-z0-9-]{1,31}$" },
    "sshKey": { "type": "string", "maxLength": 4096, "description": "One OpenSSH public key line" },
    "admin": { "type": "boolean", "default": false }
  }
}`)

	usersCreateOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["user", "uid", "admin"],
  "properties": {
    "user": { "type": "string" },
    "uid": { "type": "integer" },
    "admin": { "type": "boolean" }
  }
}`)

	usersListInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {}
}`)

	usersListOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["users"],
  "properties": {
    "users": { "type": "array", "items": ` + memberDef + ` }
  }
}`)

	usersAddKeyInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "sshKey"],
  "properties": {
    "name": { "type": "string", "pattern": "^[a-z][a-z0-9-]{1,31}$" },
    "sshKey": { "type": "string", "maxLength": 4096 }
  }
}`)

	usersAddKeyOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["user", "keys"],
  "properties": {
    "user": { "type": "string" },
    "keys": { "type": "integer" }
  }
}`)

	usersRemoveInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["name"],
  "properties": {
    "name": { "type": "string", "pattern": "^[a-z][a-z0-9-]{1,31}$" }
  }
}`)

	usersRemoveOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["user", "archived"],
  "properties": {
    "user": { "type": "string" },
    "archived": { "type": "string" }
  }
}`)

	secretsSetInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["name", "value"],
  "properties": {
    "name": { "type": "string", "pattern": "^[A-Z][A-Z0-9_]{0,63}$" },
    "value": {
      "type": "string",
      "minLength": 1,
      "maxLength": 8192,
      "description": "The value, 1 to 8192 bytes, without NUL and without a line break"
    }
  }
}`)

	secretsSetOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["name", "updated"],
  "properties": {
    "name": { "type": "string" },
    "updated": { "type": "string", "format": "date-time" }
  }
}`)

	secretsListInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {}
}`)

	secretsListOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["secrets"],
  "properties": {
    "secrets": {
      "type": "array",
      "items": {
        "type": "object",
        "required": ["name", "updated"],
        "properties": {
          "name": { "type": "string" },
          "updated": { "type": "string", "format": "date-time" }
        }
      }
    }
  }
}`)

	secretsRemoveInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["name"],
  "properties": {
    "name": { "type": "string", "pattern": "^[A-Z][A-Z0-9_]{0,63}$" }
  }
}`)

	secretsRemoveOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["name", "removed"],
  "properties": {
    "name": { "type": "string" },
    "removed": { "type": "boolean" }
  }
}`)

	approvalsListInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "state": { "type": "string", "enum": ["pending", "approved", "rejected"], "default": "pending" }
  }
}`)

	approvalsListOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["approvals"],
  "properties": {
    "approvals": { "type": "array", "items": ` + approvalDef + ` }
  }
}`)

	approvalsApproveInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["id"],
  "properties": {
    "id": { "type": "string" },
    "note": { "type": "string", "maxLength": 500 }
  }
}`)

	approvalsApproveOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["id", "state", "result"],
  "properties": {
    "id": { "type": "string" },
    "state": { "type": "string", "enum": ["approved"] },
    "result": { "type": "object" }
  }
}`)

	approvalsRejectInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["id", "reason"],
  "properties": {
    "id": { "type": "string" },
    "reason": { "type": "string", "minLength": 3, "maxLength": 500 }
  }
}`)

	approvalsRejectOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["id", "state"],
  "properties": {
    "id": { "type": "string" },
    "state": { "type": "string", "enum": ["rejected"] }
  }
}`)
)
