/* mcp-switchboard console.
 *
 * Plain DOM, fetch and EventSource. No build step, no dependencies: the hub is
 * meant to run on a loopback-only box where `npm install` is not an option.
 *
 * Layout of this file:
 *   1. helpers        small DOM/format utilities
 *   2. api            fetch wrappers
 *   3. state          everything the views render from
 *   4. tree view      connections -> servers -> tools
 *   5. tool pane      schema form, raw JSON, call + result
 *   6. calls view     filter table + detail
 *   7. events         SSE wiring
 *   8. boot           routing and first load
 */
(function () {
  "use strict";

  /* ----------------------------------------------------------- 1. helpers */

  var $ = function (sel, root) { return (root || document).querySelector(sel); };

  function el(tag, props, children) {
    var node = document.createElement(tag);
    props = props || {};
    Object.keys(props).forEach(function (key) {
      var value = props[key];
      if (value === null || value === undefined || value === false) return;
      if (key === "class") node.className = value;
      else if (key === "text") node.textContent = value;
      else if (key === "dataset") Object.assign(node.dataset, value);
      else if (key.slice(0, 2) === "on") node.addEventListener(key.slice(2), value);
      else if (value === true) node.setAttribute(key, "");
      else node.setAttribute(key, value);
    });
    append(node, children);
    return node;
  }

  function append(node, children) {
    if (children === null || children === undefined || children === false) return;
    if (Array.isArray(children)) {
      children.forEach(function (child) { append(node, child); });
      return;
    }
    node.appendChild(children.nodeType ? children : document.createTextNode(String(children)));
  }

  function clear(node) { while (node.firstChild) node.removeChild(node.firstChild); }

  function pill(kind, label) {
    return el("span", { class: "pill " + (kind || ""), text: label === undefined ? kind : label });
  }

  function pretty(value) {
    if (value === undefined) return "";
    try { return JSON.stringify(value, null, 2); } catch (err) { return String(value); }
  }

  function jsonBlock(value) {
    return el("pre", { class: "json", text: typeof value === "string" ? value : pretty(value) });
  }

  function fmtTime(iso) {
    if (!iso) return "-";
    var when = new Date(iso);
    if (isNaN(when.getTime())) return String(iso);
    var time = when.toLocaleTimeString(undefined, { hour12: false });
    var today = new Date();
    var sameDay = when.toDateString() === today.toDateString();
    return sameDay ? time : when.toLocaleDateString(undefined, { month: "short", day: "numeric" }) + " " + time;
  }

  function fmtDuration(ms) {
    if (ms === null || ms === undefined) return "-";
    if (ms < 1000) return Math.round(ms) + " ms";
    return (ms / 1000).toFixed(2) + " s";
  }

  function fmtAge(iso) {
    var when = new Date(iso);
    if (!iso || isNaN(when.getTime())) return "";
    var secs = Math.max(0, (Date.now() - when.getTime()) / 1000);
    if (secs < 90) return Math.round(secs) + "s";
    if (secs < 5400) return Math.round(secs / 60) + "m";
    if (secs < 172800) return Math.round(secs / 3600) + "h";
    return Math.round(secs / 86400) + "d";
  }

  var toastTimer = null;
  function toast(message) {
    var node = $("#toast");
    node.textContent = message;
    node.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(function () { node.hidden = true; }, 5000);
  }

  /* --------------------------------------------------------------- 2. api */

  // Relative URLs throughout, so the console survives being mounted behind a
  // reverse proxy on a sub-path.
  function request(method, path, body) {
    var init = { method: method, headers: {} };
    if (body !== undefined) {
      init.headers["content-type"] = "application/json";
      init.body = JSON.stringify(body);
    }
    return fetch(path, init).then(function (res) {
      if (res.status === 204) return null;
      return res.text().then(function (text) {
        var data = null;
        if (text) { try { data = JSON.parse(text); } catch (err) { data = text; } }
        if (!res.ok) {
          var detail = data && data.detail ? data.detail : (typeof data === "string" && data) || res.statusText;
          var error = new Error(res.status + " " + detail);
          error.status = res.status;
          throw error;
        }
        return data;
      });
    });
  }

  var api = {
    snapshot: function () { return request("GET", "api/connections"); },
    calls: function (query) { return request("GET", "api/calls?" + query); },
    call: function (conn, server, tool, args) {
      return request("POST", "api/connections/" + encodeURIComponent(conn) +
        "/servers/" + encodeURIComponent(server) +
        "/tools/" + encodeURIComponent(tool) + "/call", { arguments: args });
    },
    restart: function (conn, server) {
      return request("POST", "api/connections/" + encodeURIComponent(conn) +
        "/servers/" + encodeURIComponent(server) + "/restart");
    }
  };

  /* ------------------------------------------------------------- 3. state */

  var state = {
    view: "connections",
    snapshot: { connections: [] },
    treeFilter: "",
    collapsed: {},          // node key -> true (everything is open by default)
    selection: null,        // {connectionId, label, server, tool}
    editors: {},            // selection key -> {mode, json}
    results: {},            // selection key -> call record (or a synthetic failure)
    calls: [],
    callsLoaded: false,
    callFilters: { label: "", server: "", tool: "", status: "", limit: 100 },
    selectedCallId: null
  };

  function selKey(sel) {
    return sel ? sel.connectionId + "\u0000" + sel.server + "\u0000" + sel.tool : "";
  }

  function findTool(sel) {
    if (!sel) return null;
    var conns = state.snapshot.connections || [];
    for (var i = 0; i < conns.length; i++) {
      if (conns[i].id !== sel.connectionId) continue;
      var servers = conns[i].servers || [];
      for (var j = 0; j < servers.length; j++) {
        if (servers[j].name !== sel.server) continue;
        var tools = servers[j].tools || [];
        for (var k = 0; k < tools.length; k++) {
          if (tools[k].name === sel.tool) {
            return { connection: conns[i], server: servers[j], tool: tools[k] };
          }
        }
        return { connection: conns[i], server: servers[j], tool: null };
      }
      return { connection: conns[i], server: null, tool: null };
    }
    return null;
  }

  /* --------------------------------------------------------- 4. tree view */

  function matches(text, needle) {
    return String(text || "").toLowerCase().indexOf(needle) !== -1;
  }

  // Returns the connections to draw, already narrowed by the filter box.
  function visibleTree() {
    var needle = state.treeFilter.trim().toLowerCase();
    var conns = state.snapshot.connections || [];
    if (!needle) return conns.map(function (c) { return { conn: c, servers: (c.servers || []).map(function (s) { return { server: s, tools: s.tools || [] }; }) }; });

    var out = [];
    conns.forEach(function (conn) {
      var connHit = matches(conn.label, needle) || matches(conn.id, needle);
      var servers = [];
      (conn.servers || []).forEach(function (server) {
        var serverHit = connHit || matches(server.name, needle);
        var tools = (server.tools || []).filter(function (tool) {
          return serverHit || matches(tool.name, needle) || matches(tool.exposedName, needle) ||
            matches(tool.description, needle);
        });
        if (serverHit || tools.length) servers.push({ server: server, tools: tools });
      });
      if (connHit || servers.length) out.push({ conn: conn, servers: servers });
    });
    return out;
  }

  function isOpen(key) {
    if (state.treeFilter.trim()) return true;   // a filtered tree is always expanded
    return !state.collapsed[key];
  }

  function toggle(key) {
    if (state.collapsed[key]) delete state.collapsed[key];
    else state.collapsed[key] = true;
    renderTree();
  }

  function rowButton(props, children) {
    var node = el("div", Object.assign({ class: "tree-row", role: "button", tabindex: "0" }, props), children);
    node.addEventListener("keydown", function (ev) {
      if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); node.click(); }
    });
    return node;
  }

  function renderTree() {
    var root = $("#tree");
    var scroll = root.scrollTop;   // a live tree redraws often; don't jump the view
    clear(root);
    var tree = visibleTree();
    var toolTotal = 0, serverTotal = 0;

    (state.snapshot.connections || []).forEach(function (conn) {
      (conn.servers || []).forEach(function (server) {
        serverTotal += 1;
        toolTotal += server.toolCount !== undefined && server.toolCount !== null
          ? server.toolCount : (server.tools || []).length;
      });
    });

    if (!tree.length) {
      root.appendChild(el("div", { class: "empty" }, el("p", {
        text: (state.snapshot.connections || []).length
          ? "Nothing matches that filter."
          : "No machines are connected. Start the client on a machine and it will appear here."
      })));
    }

    tree.forEach(function (entry) {
      root.appendChild(renderConnection(entry));
    });

    $("#tree-summary").textContent = (state.snapshot.connections || []).length + " connections · " +
      serverTotal + " servers · " + toolTotal + " tools";
    root.scrollTop = scroll;
  }

  function connMeta(conn) {
    var client = conn.client || {};
    return "id " + (conn.id || "?").slice(0, 12) + " · up " + fmtAge(conn.connectedAt) +
      (client.instance ? " · " + client.instance : "");
  }

  function renderConnection(entry) {
    var conn = entry.conn;
    var key = "c:" + conn.id;
    var open = isOpen(key);
    var client = conn.client || {};
    var box = el("div", { class: "tree-conn" });

    box.appendChild(rowButton({
      class: "tree-row conn-row",
      onclick: function () { toggle(key); }
    }, [
      el("span", { class: "twist", text: open ? "\u25bc" : "\u25b6" }),
      el("span", { class: "grow" }, [
        conn.label || conn.id,
        " ",
        el("span", { class: "sub", text: client.name ? client.name + " " + (client.version || "") : "" })
      ]),
      pill("count", (entry.servers.length) + " srv")
    ]));

    if (open) {
      box.appendChild(el("div", {
        class: "conn-meta",
        dataset: { conn: conn.id },
        text: connMeta(conn)
      }));
      var servers = el("div", { class: "tree-servers" });
      entry.servers.forEach(function (item) { servers.appendChild(renderServer(conn, item)); });
      box.appendChild(servers);
    }
    return box;
  }

  function renderServer(conn, item) {
    var server = item.server;
    var key = "s:" + conn.id + "/" + server.name;
    var open = isOpen(key);
    var count = server.toolCount !== undefined && server.toolCount !== null
      ? server.toolCount : (server.tools || []).length;
    var box = el("div", {});

    var restart = el("button", {
      class: "icon-btn",
      title: "Restart " + server.name,
      text: "\u21bb",
      onclick: function (ev) {
        ev.stopPropagation();
        restart.disabled = true;
        api.restart(conn.id, server.name).then(function () {
          toast("restart requested: " + (conn.label || conn.id) + " / " + server.name);
        }).catch(function (err) {
          toast("restart failed: " + err.message);
        }).then(function () { restart.disabled = false; });
      }
    });

    box.appendChild(rowButton({
      class: "tree-row server-row",
      onclick: function () { toggle(key); }
    }, [
      el("span", { class: "twist", text: open ? "\u25bc" : "\u25b6" }),
      el("span", { class: "grow", text: server.name }),
      pill(server.state || "", server.state || "unknown"),
      pill("count", count + ""),
      restart
    ]));

    if (server.error) {
      box.appendChild(el("div", { class: "server-error", text: server.error }));
    }

    if (open) {
      var tools = el("div", { class: "tree-tools" });
      if (!item.tools.length) {
        tools.appendChild(el("div", { class: "tree-note", text: "no tools exposed" }));
      }
      item.tools.forEach(function (tool) {
        var sel = { connectionId: conn.id, label: conn.label, server: server.name, tool: tool.name };
        var selected = selKey(state.selection) === selKey(sel);
        tools.appendChild(el("button", {
          class: "tool-row" + (selected ? " selected" : ""),
          type: "button",
          title: tool.exposedName || tool.name,
          text: tool.name,
          onclick: function () { selectTool(sel); }
        }));
      });
      box.appendChild(tools);
    }
    return box;
  }

  function selectTool(sel) {
    state.selection = sel;
    renderTree();
    renderToolPane();
  }

  /* --------------------------------------------------------- 5. tool pane */

  // A schema property is only turned into a real form control when it maps
  // cleanly onto one; anything else (arrays, nested objects, oneOf soup) gets a
  // per-field JSON box, and the whole-body JSON editor is always one click away.
  function specType(spec) {
    var type = spec.type;
    if (!type && Array.isArray(spec.anyOf || spec.oneOf)) {
      var alt = (spec.anyOf || spec.oneOf).filter(function (s) { return s && s.type && s.type !== "null"; })[0];
      if (alt) type = alt.type;
    }
    if (Array.isArray(type)) {
      type = type.filter(function (t) { return t !== "null"; })[0];
    }
    return type;
  }

  function fieldKind(spec) {
    if (Array.isArray(spec.enum)) return "enum";
    var type = specType(spec);
    if (type === "boolean") return "bool";
    if (type === "number" || type === "integer") return "number";
    if (type === "string") return "string";
    return "json";
  }

  function schemaFields(schema) {
    if (!schema || typeof schema !== "object") return [];
    var props = schema.properties;
    if (!props || typeof props !== "object") return [];
    var required = Array.isArray(schema.required) ? schema.required : [];
    return Object.keys(props).map(function (name) {
      var spec = props[name] || {};
      return {
        name: name,
        spec: spec,
        kind: fieldKind(spec),
        type: specType(spec) || (Array.isArray(spec.enum) ? "enum" : "any"),
        required: required.indexOf(name) !== -1
      };
    });
  }

  function editorState(key) {
    if (!state.editors[key]) state.editors[key] = { mode: "form", json: "{}" };
    return state.editors[key];
  }

  function renderToolPane() {
    var pane = $("#tool-pane");
    clear(pane);
    var sel = state.selection;
    if (!sel) {
      pane.appendChild(el("div", { class: "empty" }, [
        el("h2", { text: "Pick a tool" }),
        el("p", { text: "Machines are on the left, with the MCP servers they carry and the tools each one exposes. Select a tool to read its schema and call it by hand." })
      ]));
      return;
    }

    var found = findTool(sel);
    var tool = found && found.tool;
    var server = found && found.server;
    var key = selKey(sel);
    var ed = editorState(key);
    var fields = tool ? schemaFields(tool.inputSchema) : [];

    /* header */
    var head = el("div", { class: "tool-head" }, [
      el("h2", { text: sel.tool }),
      el("div", { class: "path", text: (sel.label || sel.connectionId) + "  \u203a  " + sel.server }),
      el("div", {}, el("code", { class: "exposed", text: (tool && tool.exposedName) || "" })),
      tool && tool.description ? el("p", { class: "desc", text: tool.description }) : null,
      !tool ? el("p", { class: "server-error missing", text: "This tool is no longer exposed by the hub — the machine may have disconnected or the server restarted with a different tool list." }) : null,
      server && server.state && server.state !== "running"
        ? el("p", { class: "server-error", text: "server state: " + server.state + (server.error ? " — " + server.error : "") })
        : null
    ]);
    pane.appendChild(head);

    var body = el("div", { class: "tool-body" });
    pane.appendChild(body);

    /* arguments */
    var formBox = el("div", { class: "form-box" });
    var jsonBox = el("div", { class: "json-box" });
    var jsonArea = el("textarea", { class: "json-editor", spellcheck: "false" });
    var jsonError = el("div", { class: "json-error" });
    jsonArea.value = ed.json;
    jsonBox.appendChild(jsonArea);
    jsonBox.appendChild(jsonError);

    var formBtn = el("button", { type: "button", text: "Form" });
    var jsonBtn = el("button", { type: "button", text: "Raw JSON" });
    var canForm = fields.length > 0;

    function applyMode() {
      formBtn.className = ed.mode === "form" ? "on" : "";
      jsonBtn.className = ed.mode === "json" ? "on" : "";
      formBox.hidden = ed.mode !== "form";
      jsonBox.hidden = ed.mode === "form";
    }

    formBtn.disabled = !canForm;
    formBtn.title = canForm ? "" : "This tool's schema has no simple properties to build a form from.";
    if (!canForm) ed.mode = "json";

    formBtn.addEventListener("click", function () {
      // Coming back from JSON: keep whatever the user typed, dropping only the
      // keys the form cannot represent (and saying so).
      if (ed.mode === "json") {
        var parsed;
        try { parsed = JSON.parse(jsonArea.value || "{}"); } catch (err) {
          jsonError.textContent = "invalid JSON: " + err.message;
          return;
        }
        if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) {
          jsonError.textContent = "arguments must be a JSON object";
          return;
        }
        var known = fields.map(function (f) { return f.name; });
        var extra = Object.keys(parsed).filter(function (k) { return known.indexOf(k) === -1; });
        fillForm(formBox, fields, parsed);
        if (extra.length) toast("dropped keys not in the schema: " + extra.join(", "));
      }
      ed.mode = "form";
      applyMode();
    });

    jsonBtn.addEventListener("click", function () {
      if (ed.mode === "form") {
        var collected = collectForm(formBox, fields);
        jsonArea.value = pretty(collected.args);
        jsonError.textContent = "";
      }
      ed.mode = "json";
      ed.json = jsonArea.value;
      applyMode();
    });

    jsonArea.addEventListener("input", function () {
      ed.json = jsonArea.value;
      jsonError.textContent = "";
    });

    body.appendChild(el("div", { class: "section-title" }, [
      "Arguments",
      el("span", { class: "line" }),
      el("div", { class: "mode-toggle" }, [formBtn, jsonBtn])
    ]));

    buildForm(formBox, fields);
    body.appendChild(formBox);
    body.appendChild(jsonBox);
    applyMode();

    /* run */
    var runBtn = el("button", { class: "primary", type: "button", text: "Call tool" });
    var resultBox = el("div", { class: "result-box" });
    runBtn.addEventListener("click", function () {
      var args;
      if (ed.mode === "json") {
        try { args = JSON.parse(jsonArea.value || "{}"); } catch (err) {
          jsonError.textContent = "invalid JSON: " + err.message;
          return;
        }
        if (!args || typeof args !== "object" || Array.isArray(args)) {
          jsonError.textContent = "arguments must be a JSON object";
          return;
        }
      } else {
        var collected = collectForm(formBox, fields);
        if (collected.errors.length) { toast(collected.errors[0]); return; }
        args = collected.args;
      }
      runBtn.disabled = true;
      runBtn.textContent = "Calling…";
      api.call(sel.connectionId, sel.server, sel.tool, args).then(function (record) {
        state.results[key] = record;
      }).catch(function (err) {
        // A transport/route failure is not a tool result; label it apart so the
        // console never passes one off as the other.
        state.results[key] = {
          status: "failed", error: err.message, arguments: args,
          startedAt: new Date().toISOString(), transport: true
        };
      }).then(function () {
        runBtn.disabled = false;
        runBtn.textContent = "Call tool";
        renderResult(resultBox, key);
      });
    });

    body.appendChild(el("div", { class: "run-bar" }, [
      runBtn,
      el("button", {
        type: "button", text: "Clear", onclick: function () {
          delete state.results[key];
          state.editors[key] = { mode: canForm ? "form" : "json", json: "{}" };
          renderToolPane();
        }
      }),
      el("span", { class: "spacer" }),
      el("button", {
        type: "button", text: "Copy exposed name", onclick: function () {
          var name = (tool && tool.exposedName) || "";
          if (navigator.clipboard) navigator.clipboard.writeText(name).then(function () { toast("copied " + name); },
            function () { toast(name); });
          else toast(name);
        }
      })
    ]));

    body.appendChild(resultBox);
    renderResult(resultBox, key);

    if (tool && tool.inputSchema) {
      body.appendChild(el("details", { class: "schema" }, [
        el("summary", { text: "Input schema" }),
        jsonBlock(tool.inputSchema)
      ]));
    }
  }

  function buildForm(box, fields) {
    clear(box);
    if (!fields.length) {
      box.appendChild(el("p", { class: "hint", text: "No object properties in this tool's schema — use raw JSON." }));
      return;
    }
    fields.forEach(function (field) {
      box.appendChild(renderField(field));
    });
  }

  function renderField(field) {
    var spec = field.spec;
    var control;
    if (field.kind === "enum") {
      control = el("select", { "data-field": field.name });
      if (!field.required) control.appendChild(el("option", { value: "", text: "(unset)" }));
      spec.enum.forEach(function (choice) {
        control.appendChild(el("option", { value: String(choice), text: String(choice) }));
      });
    } else if (field.kind === "bool") {
      // A tri-state select, not a checkbox: for an optional boolean, "absent"
      // and "false" are different requests and a checkbox cannot say which.
      control = el("select", { "data-field": field.name });
      if (!field.required) control.appendChild(el("option", { value: "", text: "(unset)" }));
      control.appendChild(el("option", { value: "true", text: "true" }));
      control.appendChild(el("option", { value: "false", text: "false" }));
    } else if (field.kind === "number") {
      control = el("input", {
        type: "number", "data-field": field.name,
        step: specType(spec) === "integer" ? "1" : "any"
      });
    } else if (field.kind === "string") {
      control = el("input", { type: "text", "data-field": field.name, placeholder: spec.format || "" });
    } else {
      control = el("textarea", {
        "data-field": field.name, rows: "3", spellcheck: "false",
        placeholder: (field.type || "json") + " as JSON"
      });
    }

    if (spec.default !== undefined) {
      control.value = field.kind === "json" || typeof spec.default === "object"
        ? pretty(spec.default) : String(spec.default);
    }

    return el("div", { class: "field " + field.kind }, [
      el("label", {}, [
        field.name,
        field.required ? el("span", { class: "req", text: " *" }) : null,
        el("span", { class: "type", text: "  " + (field.type || "any") })
      ]),
      control,
      spec.description ? el("div", { class: "hint", text: spec.description }) : null
    ]);
  }

  function fillForm(box, fields, values) {
    fields.forEach(function (field) {
      var control = box.querySelector('[data-field="' + cssEscape(field.name) + '"]');
      if (!control) return;
      var value = values[field.name];
      if (value === undefined) { control.value = ""; return; }
      if (field.kind === "json" || (value !== null && typeof value === "object")) control.value = pretty(value);
      else control.value = String(value);
    });
  }

  function cssEscape(value) {
    if (window.CSS && CSS.escape) return CSS.escape(value);
    return String(value).replace(/["\\]/g, "\\$&");
  }

  function collectForm(box, fields) {
    var args = {};
    var errors = [];
    fields.forEach(function (field) {
      var control = box.querySelector('[data-field="' + cssEscape(field.name) + '"]');
      if (!control) return;
      var raw = control.value;
      if (raw === "" || raw === null) {
        // Absent beats empty: only send a key the user actually filled in.
        if (field.required && field.kind === "string") args[field.name] = "";
        else if (field.required) errors.push("missing required argument: " + field.name);
        return;
      }
      if (field.kind === "bool") args[field.name] = raw === "true";
      else if (field.kind === "number") {
        var num = Number(raw);
        if (isNaN(num)) errors.push(field.name + " is not a number");
        else args[field.name] = num;
      } else if (field.kind === "json") {
        try { args[field.name] = JSON.parse(raw); }
        catch (err) { errors.push(field.name + ": invalid JSON (" + err.message + ")"); }
      } else if (field.kind === "enum") {
        var match = (field.spec.enum || []).filter(function (c) { return String(c) === raw; })[0];
        args[field.name] = match === undefined ? raw : match;
      } else {
        args[field.name] = raw;
      }
    });
    return { args: args, errors: errors };
  }

  function renderResult(box, key) {
    clear(box);
    var record = state.results[key];
    if (!record) return;
    var failed = record.status !== "ok";
    box.appendChild(el("div", { class: "section-title" }, ["Result", el("span", { class: "line" })]));
    var wrap = el("div", { class: "result" + (failed ? " error" : "") });
    wrap.appendChild(el("div", { class: "result-head" }, [
      pill(record.status === "ok" ? "ok" : "error",
        record.transport ? "request failed" : (record.status || "unknown")),
      el("span", { class: "when", text: fmtTime(record.startedAt) + " · " + fmtDuration(record.durationMs) }),
      record.id ? el("span", { class: "when", text: "#" + String(record.id).slice(0, 8) }) : null
    ]));
    if (record.error !== undefined && record.error !== null) {
      wrap.appendChild(el("div", { class: "hint", text: "error" }));
      wrap.appendChild(jsonBlock(record.error));
    }
    if (record.result !== undefined && record.result !== null) {
      wrap.appendChild(el("div", { class: "hint", text: "result" }));
      wrap.appendChild(jsonBlock(record.result));
    }
    wrap.appendChild(el("div", { class: "hint", text: "arguments sent" }));
    wrap.appendChild(jsonBlock(record.arguments === undefined ? {} : record.arguments));
    box.appendChild(wrap);
  }

  /* -------------------------------------------------------- 6. calls view */

  function readCallFilters() {
    var form = $("#call-filters");
    state.callFilters = {
      label: form.label.value.trim(),
      server: form.server.value.trim(),
      tool: form.tool.value.trim(),
      status: form.status.value,
      limit: Number(form.limit.value) || 100
    };
  }

  function loadCalls() {
    var f = state.callFilters;
    var params = new URLSearchParams();
    params.set("limit", String(f.limit));
    ["label", "server", "tool", "status"].forEach(function (name) {
      if (f[name]) params.set(name, f[name]);
    });
    return api.calls(params.toString()).then(function (data) {
      state.calls = (data && data.calls) || [];
      state.callsLoaded = true;
      renderCallStats((data && data.stats) || {});
      renderCalls();
    }).catch(function (err) { toast("could not load calls: " + err.message); });
  }

  function renderCallStats(stats) {
    var parts = Object.keys(stats || {}).map(function (key) {
      var value = stats[key];
      if (value !== null && typeof value === "object") return null;
      var label = key.replace(/([a-z])([A-Z])/g, "$1 $2").toLowerCase();
      if (typeof value === "number" && !Number.isInteger(value)) value = Math.round(value * 100) / 100;
      return label + " " + value;
    }).filter(Boolean);
    $("#call-stats").textContent = parts.join("  ·  ");
  }

  function renderCalls() {
    var body = $("#calls-rows");
    clear(body);
    $("#calls-empty").hidden = state.calls.length > 0;
    state.calls.forEach(function (record) {
      body.appendChild(callRow(record));
    });
    if (state.selectedCallId) {
      var current = state.calls.filter(function (c) { return c.id === state.selectedCallId; })[0];
      if (current) renderCallDetail(current);
    }
  }

  function callRow(record, fresh) {
    var row = el("tr", {
      class: (record.id === state.selectedCallId ? "selected" : "") + (fresh ? " fresh" : ""),
      onclick: function () {
        state.selectedCallId = record.id;
        renderCalls();
        renderCallDetail(record);
      }
    }, [
      el("td", { text: fmtTime(record.startedAt) }),
      el("td", { text: record.label || "" }),
      el("td", { text: record.server || "" }),
      el("td", { class: "grow", text: record.tool || "", title: record.exposedName || "" }),
      el("td", {}, pill(record.status === "ok" ? "ok" : "error", record.status || "?")),
      el("td", { class: "num", text: fmtDuration(record.durationMs) }),
      el("td", { text: record.source || "" })
    ]);
    return row;
  }

  function renderCallDetail(record) {
    var pane = $("#call-detail");
    pane.hidden = false;
    clear(pane);
    pane.appendChild(el("div", { class: "tool-head" }, [
      el("h2", { text: record.tool || "call" }),
      el("div", { class: "path", text: (record.label || record.connectionId || "?") + "  \u203a  " + (record.server || "?") }),
      el("div", {}, el("code", { class: "exposed", text: record.exposedName || "" })),
      el("div", { class: "result-head" }, [
        pill(record.status === "ok" ? "ok" : "error", record.status || "?"),
        el("span", { class: "when", text: fmtTime(record.startedAt) + " · " + fmtDuration(record.durationMs) + " · " + (record.source || "?") })
      ])
    ]));

    var body = el("div", { class: "tool-body" });
    body.appendChild(el("div", { class: "run-bar" }, [
      el("button", {
        type: "button", text: "Open in tool pane",
        onclick: function () { openCallInToolPane(record); }
      }),
      el("span", { class: "spacer" }),
      el("button", {
        type: "button", text: "Close",
        onclick: function () { state.selectedCallId = null; pane.hidden = true; renderCalls(); }
      })
    ]));
    body.appendChild(el("div", { class: "hint", text: "arguments" }));
    body.appendChild(jsonBlock(record.arguments === undefined ? {} : record.arguments));
    if (record.error !== undefined && record.error !== null) {
      body.appendChild(el("div", { class: "hint", text: "error" }));
      body.appendChild(jsonBlock(record.error));
    }
    if (record.result !== undefined && record.result !== null) {
      body.appendChild(el("div", { class: "hint", text: "result" }));
      body.appendChild(jsonBlock(record.result));
    }
    body.appendChild(el("div", { class: "hint", text: "call id " + (record.id || "?") + " · connection " + (record.connectionId || "?") }));
    pane.appendChild(body);
  }

  // Re-running a past call is the common reason to look one up, so hand the
  // arguments straight to the tool pane instead of making people retype them.
  function openCallInToolPane(record) {
    var sel = { connectionId: record.connectionId, label: record.label, server: record.server, tool: record.tool };
    if (!findTool(sel) || !findTool(sel).tool) {
      toast("that tool is not currently connected — showing arguments only");
    }
    var key = selKey(sel);
    state.editors[key] = { mode: "json", json: pretty(record.arguments === undefined ? {} : record.arguments) };
    state.selection = sel;
    navigate("connections");
    renderTree();
    renderToolPane();
  }

  function matchesCallFilters(record) {
    var f = state.callFilters;
    // Substring matching, deliberately looser than whatever the store does, so
    // a live row is never hidden from a filter that the server would have kept.
    function ok(value, needle) {
      return !needle || String(value || "").toLowerCase().indexOf(needle.toLowerCase()) !== -1;
    }
    return ok(record.label, f.label) && ok(record.server, f.server) &&
      ok(record.tool, f.tool) && (!f.status || record.status === f.status);
  }

  function onCallEvent(record) {
    if (!state.callsLoaded || !matchesCallFilters(record)) return;
    state.calls.unshift(record);
    if (state.calls.length > state.callFilters.limit) state.calls.length = state.callFilters.limit;
    var body = $("#calls-rows");
    var row = callRow(record, true);
    if (body.firstChild) body.insertBefore(row, body.firstChild);
    else body.appendChild(row);
    while (body.children.length > state.callFilters.limit) body.removeChild(body.lastChild);
    $("#calls-empty").hidden = true;
  }

  /* ------------------------------------------------------------ 7. events */

  var refreshTimer = null;
  function refreshSnapshot() {
    return api.snapshot().then(function (data) {
      state.snapshot = data || { connections: [] };
      renderTree();
      syncToolPane();
    }).catch(function (err) { toast("could not load connections: " + err.message); });
  }

  // Debounced: a client reconnecting can fire several tree events in a row.
  function scheduleRefresh() {
    clearTimeout(refreshTimer);
    refreshTimer = setTimeout(refreshSnapshot, 150);
  }

  // Redrawing the whole tool pane on every tree change would wipe out whatever
  // the user is typing, so only the header is refreshed unless the tool is gone.
  function syncToolPane() {
    if (!state.selection) return;
    var found = findTool(state.selection);
    var missing = !found || !found.tool;
    var notice = $("#tool-pane").querySelector(".tool-head .missing");
    if (missing !== !!notice) renderToolPane();
  }

  function setLive(status, text) {
    var box = $("#live");
    box.className = "live " + status;
    $("#live-text").textContent = text;
  }

  function connectEvents() {
    var source = new EventSource("api/events");
    source.onopen = function () { setLive("up", "live"); };
    source.onerror = function () {
      // EventSource reconnects on its own; just report the gap.
      setLive("down", "reconnecting");
    };
    source.onmessage = function (event) {
      var data;
      try { data = JSON.parse(event.data); } catch (err) { return; }
      setLive("up", "live");
      if (data.type === "connections") scheduleRefresh();
      else if (data.type === "call" && data.call) onCallEvent(data.call);
    };
  }

  /* -------------------------------------------------------------- 8. boot */

  function navigate(view) {
    state.view = view === "calls" ? "calls" : "connections";
    $("#view-connections").hidden = state.view !== "connections";
    $("#view-calls").hidden = state.view !== "calls";
    Array.prototype.forEach.call(document.querySelectorAll("#tabs a"), function (link) {
      link.classList.toggle("active", link.dataset.view === state.view);
    });
    if (window.location.hash !== "#/" + state.view) window.location.hash = "#/" + state.view;
    if (state.view === "calls" && !state.callsLoaded) loadCalls();
  }

  function fromHash() {
    navigate((window.location.hash || "").replace(/^#\/?/, "") || "connections");
  }

  function boot() {
    $("#tree-filter").addEventListener("input", function (ev) {
      state.treeFilter = ev.target.value;
      renderTree();
    });
    $("#refresh-tree").addEventListener("click", refreshSnapshot);

    $("#call-filters").addEventListener("submit", function (ev) {
      ev.preventDefault();
      readCallFilters();
      loadCalls();
    });
    $("#calls-reset").addEventListener("click", function () {
      $("#call-filters").reset();
      readCallFilters();
      loadCalls();
    });

    readCallFilters();
    window.addEventListener("hashchange", fromHash);
    fromHash();
    refreshSnapshot();
    renderToolPane();
    connectEvents();
    setInterval(refreshAges, 30000);
  }

  // Uptime is the only thing that goes stale without an event, so tick just
  // those labels rather than rebuilding the tree under the user's cursor.
  function refreshAges() {
    var byId = {};
    (state.snapshot.connections || []).forEach(function (conn) { byId[conn.id] = conn; });
    Array.prototype.forEach.call(document.querySelectorAll(".conn-meta"), function (node) {
      var conn = byId[node.dataset.conn];
      if (conn) node.textContent = connMeta(conn);
    });
  }

  document.addEventListener("DOMContentLoaded", boot);
})();
