# Switchboard: selection to chat

One command: quote the current editor selection into a Switchboard chat's
**draft** (the composer of an open console tab updates live — the hub publishes
a draft event on every write). The citation is explicit and attributable — it
becomes part of the message you next send, never a silent prompt edit:

    mcp-switchboard:client/src/mcp_switchboard_client/tunnel.py:501-520
    ```python mcp-switchboard:client/src/mcp_switchboard_client/tunnel.py:501-520
    ...selected lines...
    ```

`Ctrl+Alt+L` (Cmd+Alt+L on macOS) with a selection, or right-click -> *Switchboard:
Send Selection to Chat Draft*. The first send of a session asks which chat; the
choice is remembered.

It talks to the hub's **private** listener (`http://127.0.0.1:8099`) directly —
no tunnel, no protocol frame, no hub change. Set `switchboard.token` if the hub
runs with `MCP_SWITCHBOARD_PRIVATE_TOKEN`.

## Install

Plain JavaScript, no build step.

- From a terminal:

  ```sh
  npx --yes @vscode/vsce package   # needs network for the first run -> .vsix
  code --install-extension switchboard-citations-0.0.1.vsix
  ```

- Without packaging (fine on a personal machine): symlink the folder into the
  extensions directory and reload the window:

  ```sh
  ln -s "$PWD" ~/.vscode/extensions/mcp-switchboard.switchboard-citations
  ```

Try it without installing anything: `code --extensionDevelopmentPath=$PWD`
opens a dev window with the command registered.
