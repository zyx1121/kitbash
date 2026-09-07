package server

import "encoding/json"

// The schemas below are the fs family of spec/mcp-surface.yaml, transcribed to
// JSON Schema 2020-12. They are declared here rather than inferred from Go
// types so the wire contract stays the one the spec publishes.

const folderEntryDef = `{
  "type": "object",
  "required": ["path", "name", "description"],
  "properties": {
    "path": { "type": "string" },
    "name": { "type": "string" },
    "description": { "type": "string" },
    "tags": { "type": "array", "items": { "type": "string" } },
    "package": { "type": "boolean", "description": "true when the manifest carries a deploy block" }
  }
}`

const fileEntryDef = `{
  "type": "object",
  "required": ["path", "name", "size", "mediaType"],
  "properties": {
    "path": { "type": "string" },
    "name": { "type": "string" },
    "size": { "type": "integer" },
    "mediaType": { "type": "string", "description": "IANA media type guessed from extension and sniffing" },
    "modified": { "type": "string", "format": "date-time" }
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
  "required": ["path", "folders", "files"],
  "properties": {
    "path": { "type": "string" },
    "manifest": { "type": "object", "description": "This folder's parsed kitbash.yaml, absent at the roots" },
    "folders": { "type": "array", "items": ` + folderEntryDef + ` },
    "files": { "type": "array", "items": ` + fileEntryDef + ` }
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
	// fs.ReadMeta, and it is documented in spec/mcp-surface.yaml.

	writeInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "required": ["path", "message"],
  "oneOf": [
    { "required": ["content"] },
    { "required": ["contentBase64"] }
  ],
  "properties": {
    "path": { "type": "string" },
    "content": { "type": "string", "description": "UTF-8 text" },
    "contentBase64": { "type": "string", "contentEncoding": "base64" },
    "message": { "type": "string", "minLength": 3, "maxLength": 200, "description": "Commit message, imperative mood" },
    "expectedSha": { "type": "string", "description": "Last known commit sha for this file; optimistic lock" }
  }
}`)

	writeOutputSchema = json.RawMessage(`{
  "type": "object",
  "required": ["path", "commit"],
  "properties": {
    "path": { "type": "string" },
    "commit": ` + commitDef + `
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
    "builtAt": { "type": "string", "format": "date-time" }
  }
}`

const processDef = `{
  "type": "object",
  "required": ["id", "name", "package", "digest", "state"],
  "properties": {
    "id": { "type": "string" },
    "name": { "type": "string" },
    "package": { "type": "string" },
    "digest": { "type": "string" },
    "state": { "type": "string", "enum": ["starting", "running", "unhealthy", "stopped", "failed"] },
    "expose": { "type": "string", "enum": ["mcp", "http", "none"] },
    "startedAt": { "type": "string", "format": "date-time" }
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
          "digest": { "type": "string" },
          "builtAt": { "type": "string", "format": "date-time" },
          "running": { "type": "integer", "description": "Number of Processes of this Package" }
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
    "digest": { "type": "string", "pattern": "^sha256:[a-f0-9]{64}$", "description": "Defaults to the latest build" },
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
    "state": { "type": "string", "enum": ["starting", "running", "unhealthy", "stopped", "failed"] },
    "expose": { "type": "string", "enum": ["mcp", "http", "none"] },
    "endpoint": { "type": "string", "description": "Internal URL when expose is http" },
    "tools": { "type": "array", "items": { "type": "string" }, "description": "Surface tool names added when expose is mcp" }
  }
}`)

	procListInputSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {}
}`)

	procListOutputSchema = json.RawMessage(`{
  "type": "object",
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
  "description": "The four attributes plus tool and eval, in short form. Absent when the record did not carry them.",
  "properties": {
    "user": { "type": "string" },
    "package": { "type": "string" },
    "process": { "type": "string" },
    "path": { "type": "string" },
    "tool": { "type": "string" },
    "eval": { "type": "boolean" },
    "producer": { "type": "string", "description": "Member name or Process id that wrote the record" },
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
