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

	readOutputSchema = json.RawMessage(`{
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
