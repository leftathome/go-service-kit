// Self-hosted OpenAPI reference renderer.
//
// Deliberately dependency-free: the kit's deployment target is a distroless
// image in a cluster with no guaranteed egress, and every off-the-shelf
// renderer (Stoplight Elements, Scalar, Swagger UI) is loaded from a CDN by
// default. A CDN reference turns this page into a blank screen and an outbound
// connection attempt. Everything here is served by the process itself under a
// default-src 'none' CSP.
//
// It renders the document, it does not execute requests against the API: a
// "try it" button on a page served by the API itself is a CSRF primitive.

(function () {
  "use strict";

  var METHODS = ["get", "put", "post", "delete", "patch", "options", "head", "trace"];

  function el(tag, attrs, children) {
    var node = document.createElement(tag);
    if (attrs) {
      Object.keys(attrs).forEach(function (k) {
        if (k === "class") { node.className = attrs[k]; }
        else if (k === "text") { node.textContent = attrs[k]; }
        else { node.setAttribute(k, attrs[k]); }
      });
    }
    (children || []).forEach(function (c) {
      if (c) { node.appendChild(c); }
    });
    return node;
  }

  function slug(method, path) {
    return (method + path).toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "");
  }

  // Render a schema as a short type expression, following one level of $ref.
  function typeOf(schema, doc, depth) {
    if (!schema) { return "-"; }
    depth = depth || 0;
    if (schema.$ref) {
      var name = schema.$ref.split("/").pop();
      if (depth > 2) { return name; }
      var target = ((doc.components || {}).schemas || {})[name];
      return target ? name : name;
    }
    if (schema.type === "array") {
      return typeOf(schema.items, doc, depth + 1) + "[]";
    }
    if (Array.isArray(schema.type)) { return schema.type.join(" | "); }
    if (schema.enum) { return schema.enum.map(String).join(" | "); }
    return schema.type || "object";
  }

  function resolve(schema, doc) {
    if (schema && schema.$ref) {
      var name = schema.$ref.split("/").pop();
      return ((doc.components || {}).schemas || {})[name] || schema;
    }
    return schema;
  }

  function paramTable(params, doc) {
    var rows = params.map(function (p) {
      var s = p.schema || {};
      var constraints = [];
      ["minimum", "maximum", "minLength", "maxLength", "pattern", "format"].forEach(function (k) {
        if (s[k] !== undefined) { constraints.push(k + "=" + s[k]); }
      });
      if (s.default !== undefined) { constraints.push("default=" + JSON.stringify(s.default)); }
      return el("tr", null, [
        el("td", null, [el("code", { text: p.name })]),
        el("td", { text: p.in }),
        el("td", { text: typeOf(s, doc) }),
        el("td", { class: "req", text: p.required ? "required" : "" }),
        el("td", { text: [p.description, constraints.join(", ")].filter(Boolean).join(" ") })
      ]);
    });
    return el("table", null, [
      el("thead", null, [el("tr", null, ["Name", "In", "Type", "", "Notes"].map(function (h) {
        return el("th", { text: h });
      }))]),
      el("tbody", null, rows)
    ]);
  }

  function responseTable(responses, doc) {
    var rows = Object.keys(responses).sort().map(function (code) {
      var r = responses[code] || {};
      var content = r.content || {};
      var cts = Object.keys(content);
      var schema = cts.length ? content[cts[0]].schema : null;
      return el("tr", null, [
        el("td", null, [el("code", { text: code })]),
        el("td", { text: cts.join(", ") }),
        el("td", { text: schema ? typeOf(schema, doc) : "-" }),
        el("td", { text: r.description || "" })
      ]);
    });
    return el("table", null, [
      el("thead", null, [el("tr", null, ["Status", "Content type", "Body", "Description"].map(function (h) {
        return el("th", { text: h });
      }))]),
      el("tbody", null, rows)
    ]);
  }

  function schemaBlock(schema, doc) {
    var resolved = resolve(schema, doc);
    return el("pre", { text: JSON.stringify(resolved, null, 2) });
  }

  function operationSection(path, method, op, doc) {
    var children = [
      el("h2", null, [
        el("span", { class: "badge " + method, text: method.toUpperCase() }),
        el("span", { class: "path", text: path }),
        op.operationId ? el("span", { class: "opid", text: op.operationId }) : null
      ])
    ];
    if (op.summary) { children.push(el("p", { class: "summary", text: op.summary })); }
    if (op.description) { children.push(el("p", { class: "desc", text: op.description })); }

    var params = (op.parameters || []).slice();
    if (params.length) {
      children.push(el("h3", { text: "Parameters" }));
      children.push(paramTable(params, doc));
    }

    if (op.requestBody) {
      children.push(el("h3", { text: "Request body" }));
      var rc = op.requestBody.content || {};
      Object.keys(rc).forEach(function (ct) {
        children.push(el("p", { class: "desc", text: ct }));
        children.push(schemaBlock(rc[ct].schema, doc));
      });
    }

    if (op.responses) {
      children.push(el("h3", { text: "Responses" }));
      children.push(responseTable(op.responses, doc));
    }

    return el("section", { class: "op", id: slug(method, path) }, children);
  }

  function render(doc) {
    var info = doc.info || {};
    document.getElementById("title").textContent = info.title || "API reference";
    document.title = (info.title || "API") + " reference";

    var sub = document.getElementById("subtitle");
    sub.textContent = "";
    sub.appendChild(el("span", { id: "version", text: "version " + (info.version || "unknown") }));
    if (info.description) {
      sub.appendChild(document.createTextNode(" - " + info.description));
    }

    var nav = document.getElementById("nav");
    var main = document.getElementById("main");
    var list = el("ul", null, []);
    var paths = doc.paths || {};

    Object.keys(paths).sort().forEach(function (path) {
      METHODS.forEach(function (method) {
        var op = paths[path][method];
        if (!op) { return; }
        var id = slug(method, path);
        list.appendChild(el("li", null, [
          el("a", { href: "#" + id, text: method.toUpperCase() + " " + path })
        ]));
        main.appendChild(operationSection(path, method, op, doc));
      });
    });

    if (!list.childNodes.length) {
      main.appendChild(el("p", { class: "desc", text: "This API declares no operations." }));
    }
    nav.appendChild(list);
  }

  fetch("/openapi.json", { credentials: "same-origin" })
    .then(function (r) {
      if (!r.ok) { throw new Error("GET /openapi.json returned " + r.status); }
      return r.json();
    })
    .then(render)
    .catch(function (err) {
      var sub = document.getElementById("subtitle");
      sub.className = "err";
      sub.textContent = "Could not load the OpenAPI document: " + err.message;
    });
})();
