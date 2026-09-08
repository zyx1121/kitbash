# The graph format

A workflow is a file in Files. You write it, this kit runs it. Call `workflow_run`
with the file's absolute path and, when the graph asks for one, an input object.

Every step calls one tool of your own MCP surface, the same surface you are
reading this from, or runs another graph. Steps run in the order they are
written, one at a time. A step's input may reference the graph's input and the
output of any step before it.

The file is YAML or JSON, at most 256 KiB. Write it with `fs_write` into a folder
that carries a `kitbash.yaml`, because a folder without one is not readable.

## The keys

```yaml
name: monthly report          # what this graph is, optional
description: One or two lines an agent reads before running it.   # optional
input:                        # JSON Schema of the graph's input, optional
  type: object
  required: [subject]
  properties:
    subject: { type: string }
steps:                        # required, at least one
  - id: greet                 # ^[a-z][a-z0-9_]*$, unique in this graph
    tool: echo_echo           # a tool on your surface, <package>_<tool>
    input:                    # the arguments, templated, optional
      message: "Hello ${input.subject}"
    output:                   # reshape the raw result, optional
      greeting: ${result.content.0.text}
output:                       # the graph's own output, optional
  greeting: ${steps.greet.output.greeting}
```

A step names exactly one of `tool` and `workflow`. Anything else at the top level
or inside a step is refused, so a misspelled key is an error and not a silent
no-op.

`input` documents the graph for whoever calls it. The engine does not validate
against the schema; it checks only that every name the schema lists in
`required` was passed, and refuses the run when one is missing.

## References

A reference is `${<dotted path>}`. Two roots are in scope in a step input:

- `${input.<field>}`, the object you passed to `run`.
- `${steps.<id>.output.<field>}`, the output of a step listed **before** this one.
  A reference to a later step is refused before anything runs.

A digit segment indexes an array, as in `${result.content.0.text}`.

A value that is exactly one reference keeps the referenced value's type, so
`count: ${input.n}` passes the number 7. A reference inside a longer string is
stringified into it, so `message: "n is ${input.n}"` passes the string `n is 7`.
A reference that resolves to nothing stops the run and says which segment is
missing.

## What a step returns

Without an `output` block, a step's output is the tool's structured content, or
`{"text": "<the text blocks joined>"}` when the tool returned only text.

With an `output` block, each value is a reference into the tool's raw MCP result
under the root `result`: `${result.structuredContent.<field>}` for structured
content, `${result.content.0.text}` for the first text block. The raw result of a
`workflow` step is the nested graph's output object.

## The graph's output

Without an `output` block, the graph's output is every step's output keyed by
step id. With one, it is that mapping resolved, referencing `${input...}` and
`${steps.<id>.output...}`.

## What run returns

```json
{
  "path": "/org/flows/greet.yaml",
  "outputs": { "greeting": "Echo: Hello world" },
  "steps": [{ "id": "greet", "tool": "echo_echo", "durationMs": 12, "status": "ok" }]
}
```

`steps` carries one entry per step of the graph that was run, in the order they
ran. A `workflow` step is one entry naming the graph it ran, not the steps
inside it.

## Example: a flat graph

The two tools below are the ones used throughout this file. Check their schemas
on your own surface before you copy the inputs.

```yaml
# /org/flows/sum-and-say.yaml
name: sum and say
description: Adds two numbers and says the total.
input:
  type: object
  required: [a, b]
  properties:
    a: { type: number }
    b: { type: number }
steps:
  - id: total
    tool: server-everything_get-sum
    input:
      a: ${input.a}
      b: ${input.b}

  - id: say
    tool: echo_echo
    input:
      message: "the total is ${steps.total.output.sum}"
    output:
      said: ${result.content.0.text}

output:
  sum: ${steps.total.output.sum}
  said: ${steps.say.output.said}
```

```json
{ "path": "/org/flows/sum-and-say.yaml", "input": { "a": 2, "b": 3 } }
```

## Example: a graph that runs another graph

A step with `workflow` runs the graph at that path. The step's `input` is that
graph's input, and the step's output is that graph's output. The session, the
tools and the run's time budget are shared with the parent.

```yaml
# /org/flows/report.yaml
name: report
description: Sums a pair through another graph, then says what came back.
input:
  type: object
  required: [a, b]
  properties:
    a: { type: number }
    b: { type: number }
steps:
  - id: totals
    workflow: /org/flows/sum-and-say.yaml
    input:
      a: ${input.a}
      b: ${input.b}

  - id: report
    tool: echo_echo
    input:
      message: "sum ${steps.totals.output.sum}, said ${steps.totals.output.said}"

output:
  sum: ${steps.totals.output.sum}
  report: ${steps.report.output.text}
```

## Limits and failures

- A graph file is read whole and must be at most 256 KiB.
- Graphs nest at most 8 deep, and a graph may not run itself, directly or
  through the graphs it runs.
- One tool call has 60 seconds, and one run has 5 minutes in total.
- A step whose tool fails stops the run. `run` answers that tool's own problem,
  with the step named in `detail` and `<graph path>#<step id>` in `instance`, so
  a missing file in a step is still a `not-found` and not an engine failure.
- A step naming a tool that is not on your surface is a `not-found` before the
  run starts calling it. Run the Package that provides the tool first.

## Telemetry

Every call this engine makes is a span kitbashd already records, carrying
`kitbash.caller` set to this Process. The engine adds one span per run, named
`workflow.run`, with `kitbash.path` set to the graph and
`kitbash.workflow.steps` set to the number of steps it ran. A run that failed
carries an error status. Query it like anything else with `tel_query`.
