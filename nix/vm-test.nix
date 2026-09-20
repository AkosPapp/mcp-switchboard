# End-to-end NixOS VM test: two machines, one tunnel, one real tool call.
#
# Two nodes rather than loopback on purpose. The whole premise of this project
# is that the client needs no inbound port - it dials out - so the test has to
# put a network boundary between them for that claim to mean anything. The
# client node runs a genuine stdio MCP server (tests/fake_mcp_server.py, which
# uses the real mcp SDK), so nothing in the path is stubbed: the hub performs an
# actual initialize / tools/list / tools/call against a separate process on a
# separate machine, over the tunnel.
#
# It also pins the security property the two-listener split exists for: the
# private listener (console, /api, /mcp, /metrics) must NOT be reachable from
# anywhere but the hub itself.
{
  pkgs,
  lib ? pkgs.lib,
  module,
  hubPackage,
  clientPackage,
  # A python environment that has the `mcp` SDK, for the fake stdio server.
  mcpPython,
  fakeMcpServerSource,
}:

let
  tunnelPort = 8097;
  privatePort = 8099;
  token = "vm-test-token";
  label = "vmclient";

  fakeMcpServer = pkgs.runCommand "fake-mcp-server" { } ''
    mkdir -p $out
    cp ${fakeMcpServerSource} $out/fake_mcp_server.py
  '';

  clientConfig = pkgs.writeText "mcp.json" (
    builtins.toJSON {
      mcpServers.demo = {
        command = "${mcpPython}/bin/python";
        args = [
          "${fakeMcpServer}/fake_mcp_server.py"
          "demo"
        ];
      };
    }
  );

  # writePython3Bin runs flake8 over the script. These are style-only rules that
  # fight with how the strings below are interpolated from Nix.
  pyFlags = {
    flakeIgnore = [
      "E501" # line too long
      "E241" # whitespace after ','
      "W503" # line break before binary operator (flake8 flags both sides)
      "W504" # line break after binary operator
    ];
  };

  api = "http://127.0.0.1:${toString privatePort}";

  # Exits non-zero until the hub reports vmclient/demo running with a non-empty
  # tool list whose exposed name is the one a consumer like n8n would see.
  # Written as a poll-once script so the test driver's wait_until_succeeds owns
  # the retry loop and the timeout, instead of sleeping blindly.
  awaitTools = pkgs.writers.writePython3Bin "mcpsb-await-tools" pyFlags ''
    import json
    import sys
    import urllib.request

    with urllib.request.urlopen("${api}/api/connections", timeout=5) as r:
        data = json.load(r)

    for conn in data.get("connections", []):
        if conn.get("label") != "${label}":
            continue
        for server in conn.get("servers", []):
            if server.get("name") != "demo":
                continue
            if server.get("state") != "running":
                sys.exit(f"demo is {server.get('state')}: {server.get('error')}")
            tools = server.get("tools") or []
            if not tools:
                sys.exit("demo is running but exposes no tools yet")
            exposed = sorted(t["exposedName"] for t in tools)
            expected = ["${label}__demo__boom", "${label}__demo__echo"]
            if exposed != expected:
                sys.exit(f"unexpected exposed names: {exposed} != {expected}")
            print(conn["id"])
            sys.exit(0)

    sys.exit("no connection labelled ${label} with a demo server yet")
  '';

  connId = pkgs.writers.writePython3Bin "mcpsb-connection-id" pyFlags ''
    import json
    import sys
    import urllib.request

    with urllib.request.urlopen("${api}/api/connections", timeout=5) as r:
        data = json.load(r)

    for conn in data.get("connections", []):
        if conn.get("label") == "${label}":
            print(conn["id"])
            sys.exit(0)

    sys.exit("no connection labelled ${label}")
  '';

  callEcho = pkgs.writers.writePython3Bin "mcpsb-call-echo" pyFlags ''
    import json
    import sys
    import urllib.request

    connection_id = sys.argv[1]
    url = "${api}/api/connections/{}/servers/demo/tools/echo/call".format(connection_id)
    body = json.dumps({"arguments": {"message": "hello"}}).encode()
    req = urllib.request.Request(
        url, data=body, headers={"Content-Type": "application/json"}, method="POST"
    )

    with urllib.request.urlopen(req, timeout=30) as r:
        if r.status != 200:
            sys.exit(f"expected 200, got {r.status}")
        record = json.load(r)

    print(json.dumps(record, indent=2))

    if record.get("status") != "ok":
        sys.exit(f"expected status ok, got {record.get('status')}: {record.get('error')}")
    if record.get("exposedName") != "${label}__demo__echo":
        sys.exit(f"unexpected exposedName {record.get('exposedName')}")
    if "echo: hello" not in json.dumps(record.get("result")):
        sys.exit("result did not contain 'echo: hello'")
  '';
in

pkgs.testers.runNixOSTest {
  name = "mcp-switchboard";

  nodes = {
    hub =
      { ... }:
      {
        imports = [ module ];

        services.mcp-switchboard = {
          enable = true;
          package = hubPackage;

          # Reachable from the other VM ...
          tunnel.host = "0.0.0.0";
          tunnel.port = tunnelPort;
          openFirewall = true;

          # ... while the console/API/metrics stay on loopback, which is the
          # posture this module is built around. Step 6 of the test script
          # proves it.
          private.host = "127.0.0.1";
          private.port = privatePort;

          # A literal token, which the module warns about for real deployments.
          # Fine here: it is a throwaway VM and the point is the wire protocol,
          # not secret handling.
          tunnelToken = token;

          logLevel = "DEBUG";
        };

        environment.systemPackages = [
          awaitTools
          connId
          callEcho
          pkgs.curl
        ];
      };

    client =
      { ... }:
      {
        # Deliberately does NOT import the module: the client is a plain
        # userspace process with no server role and nothing to configure
        # system-wide.
        environment.etc."mcp-switchboard/mcp.json".source = clientConfig;
        environment.systemPackages = [ pkgs.curl ];

        systemd.services.mcp-switchboard-client = {
          description = "mcp-switchboard tunnel client";
          wantedBy = [ "multi-user.target" ];
          after = [ "network-online.target" ];
          wants = [ "network-online.target" ];

          serviceConfig = {
            ExecStart = lib.concatStringsSep " " [
              "${clientPackage}/bin/mcp-switchboard-client"
              "--hub-url ws://hub:${toString tunnelPort}"
              "--token ${token}"
              "--label ${label}"
              "--config /etc/mcp-switchboard/mcp.json"
              "--log-level DEBUG"
            ];
            Restart = "on-failure";
            RestartSec = "2s";
            DynamicUser = true;
          };
        };
      };
  };

  testScript = ''
    start_all()

    with subtest("both ends come up"):
        hub.wait_for_unit("mcp-switchboard.service")
        hub.wait_for_open_port(${toString tunnelPort})
        client.wait_for_unit("mcp-switchboard-client.service")

    with subtest("the client's local MCP server is discovered through the tunnel"):
        # Polls until demo is running with a non-empty tool list and the
        # exposed names are exactly {label}__{server}__{tool}.
        hub.wait_until_succeeds("mcpsb-await-tools", timeout=180)
        connection_id = hub.succeed("mcpsb-connection-id").strip()
        assert connection_id, "hub reported no connection id"

    with subtest("a tool call round-trips to the other machine and back"):
        hub.succeed(f"mcpsb-call-echo {connection_id}")

    with subtest("the call shows up in the call log"):
        hub.succeed(
            "curl -fsS '${api}/api/calls?limit=10' | grep -q 'echo: hello'"
        )

    with subtest("prometheus metrics are exported"):
        hub.succeed("curl -fsS ${api}/metrics | grep -q mcpsb_tool_calls_total")

    with subtest("the console is served out of the binary"):
        # Nothing is fetched at runtime: the built console is embedded, so this
        # also proves the asset the index names is in there (spec.md U1).
        hub.succeed("curl -fsS ${api}/ | grep -qi '<!doctype html'")
        asset = hub.succeed(
            "curl -fsS ${api}/ | grep -o '/assets/[^\"]*\\.js' | head -1"
        ).strip()
        assert asset, "the console index named no script"
        hub.succeed(f"curl -fsS '${api}{asset}' > /dev/null")

        # A deep link is served by the hub's fallback rather than by the router
        # alone, so a reload on /calls has to work.
        hub.succeed("curl -fsS ${api}/calls | grep -qi '<!doctype html'")

    with subtest("the private listener is not reachable from the client machine"):
        # The reason the hub has two listeners at all: only the token-guarded
        # tunnel port faces the network.
        client.fail(
            "curl --max-time 5 -fsS http://hub:${toString privatePort}/api/connections"
        )
  '';
}
