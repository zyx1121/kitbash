# Handbook

This machine runs kitbash. Everything you can see through this connection lives in two places.

- `/org` is shared. Every member reads it. Writes go through an approval by an admin.
- `/home/<you>` is yours. Write freely, it is a git repository and every write is a commit.

A folder is visible only when it carries a `kitbash.yaml` with a name and a description. When you create a folder, write its manifest first.

Read [`surface.md`](surface.md) for the tools you can call, the path rules, the error shape and how to query Telemetry.
Read [`kits.md`](kits.md) before writing a Package: the manifest, the five hooks, what a Process is given, and the build and run loop.

The logo next to this file is here so image reading can be verified.
