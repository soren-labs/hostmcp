# Agent Host MCP

Build a small headless MCP host for Linux and macOS. The user explicitly wants tools
to operate with the full ordinary privileges of the OS account running the service.

- The initial commit is a source-extraction baseline, not a working MCP server.
- Treat `reference/chat-on-steroids/` as immutable upstream evidence. Adapt code into
  a new runtime tree when implementation starts; keep the provenance hashes intact.
- Preserve upstream copyright headers and the MIT/Apache attribution in `NOTICE`.
- Use real OS paths. Do not restore approved-folder policy or browser conversation
  identity as a prerequisite for ordinary filesystem and process operations.
- Full user authority does not mean automatic root elevation or bypassing macOS
  privacy permissions. Preserve OS errors and normal user environment semantics.
- Keep transport authentication, input validation, bounded output and process
  ownership separate from OS filesystem permissions.
- Target standard MCP clients over stdio and Streamable HTTP. No Electron,
  renderer, tray, browser extension, ChatGPT DOM automation, model API, or GUI.
- Do not read, copy, modify, restart, install over, or connect tests to a running
  Chat On Steroids installation or its user data. Use isolated temporary fixtures.
- Source checks: `python3 scripts/verify-source-import.py`. Runtime and Linux/macOS
  claims require their own actual tests once the new service exists.
