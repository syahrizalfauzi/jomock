# jomock

A small standalone HTTP and gRPC mock server. Point your app at it, define stubs as
JSON, get deterministic responses. WireMock-flavoured without the JVM.

Single Go binary. No database, no node, no protoc at runtime.

## Run

```sh
make run                 # mock on :8081, admin API + UI on :8080
go run . -stubs my.json  # custom stub file
go run . -proto desc.bin # also answer gRPC calls
```

Stubs live in `stubs.json` (created on first change, reloaded on start).

- **mock** — `http://localhost:8081` — put this in your app's base URL.
- **admin + UI** — `http://localhost:8080` — add, edit, reorder stubs; watch the request journal; run verify.

## UI

The admin port serves a form editor — no hand-written stub JSON needed:

- **+ New / edit** — method, path (exact or regex), query and header rows, request body
  matcher, then status, response headers, body (JSON or text) and delay.
  The JSON body box takes raw JSON, so nothing needs escaping.
- **Save** — downloads the current stubs as `stubs.json` (a snapshot you can commit).
- **Open** — loads a `stubs.json` from disk and replaces the whole list. Rejected if any
  stub is invalid; the running list is left untouched.
- **stub** — the journal marks unmatched requests with a `stub` button that opens the
  form pre-filled with that request's method, path and protocol. Query and headers are
  deliberately not copied: they are per-call values, and a stub declaring them stops
  matching as soon as your app sends one fewer.
- **Type** — pick HTTP or gRPC. The form swaps the HTTP method and status fields for the
  gRPC status codes, and the path field datalists the rpcs you imported.
- **Status** — a dropdown that says what the code means (`200 OK`, `404 Not Found`,
  `5 NOT_FOUND`, `7 PERMISSION_DENIED`). HTTP has `custom code...` for anything not
  listed; the gRPC list is the complete standard set, 0-16.
- **hide warnings** — collapse the Protos warning list; the choice is remembered.

The running server always persists to its own stub file too, so the buttons are for
snapshots and sharing, not for durability.

## Stub shape

```json
{
  "id": "optional, assigned when omitted",
  "type": "http",
  "request": {
    "method": "POST",
    "path": "/orders",
    "pathPattern": "^/orders/[0-9]+$",
    "query": { "page": "1" },
    "headers": { "X-Api-Key": "secret" },
    "body": { "equalJson": { "sku": "A1" } }
  },
  "response": {
    "status": 201,
    "headers": { "Content-Type": "application/json" },
    "jsonBody": { "id": 7 },
    "delayMs": 250
  }
}
```

Rules:

- `path` is exact; `pathPattern` is a Go regexp. Leave both out to match any path.
- `query` and `headers` are **subset** matches — declared keys must match, extra params are ignored.
- `body` picks one of `equal` (raw string), `contains` (substring), `equalJson` (semantic JSON equality).
- `jsonBody` takes any JSON value. It is serialised back to JSON at serve time and sent
  with `Content-Type: application/json` unless you set that header yourself — paste raw
  JSON, no escaping. `body` remains for literal strings; `jsonBody` wins if both are set.
- `type` is `http` (the default when omitted) or `grpc`. A stub only answers its own
  protocol, so an HTTP stub can never swallow a gRPC call that happens to share a path.
- `status` defaults to 200, `delayMs` defaults to 0.
- **First stub in the list wins.** Reorder with the up/down buttons in the UI.
- Unmatched request → `404 {"error":"no matching stub", ...}`.

## gRPC

The mock port speaks HTTP/1.1 and **h2c** (unencrypted HTTP/2) on the same address, so
gRPC clients work with plain `insecure.NewCredentials()` — no TLS, no extra port.

**Import `.proto` files in the UI — no protoc, no descriptor file.** Open the *Protos*
section on the admin port, hit **Import .proto folder**, and pick the folder that holds
your services (include the folder that holds the imported plugins, so
`google/api/annotations.proto` style imports resolve). Compilation happens in-process.

Each top-level folder is compiled on its own. A folder that does not compile is listed as
a warning and the rest still load, so one broken service in a large tree costs you nothing.

**You import once.** The uploaded sources are cached to `protos.json` and recompiled on the
next start, exactly like `stubs.json` — so a restart, a `make run`, or a machine reboot
keeps your rpcs. Set `-protos ""` to turn the cache off. The file is the same shape as the
upload body, so it is readable and hand-editable.

Faster to write but needs protoc installed:

```sh
protoc --proto_path=proto --proto_path=proto/plugins --include_imports \
  --descriptor_set_out=desc.bin $(find proto/my-service -name '*.proto')
go run . -proto desc.bin
```

Import paths are **case-sensitive**. macOS will happily resolve an import of
`google/protobuf/Empty.proto` to the lowercase file, so that typo compiles locally and
breaks on Linux and CI. jomock names the offending import in its warnings instead of
just saying "could not resolve".

A gRPC stub is an ordinary stub whose `path` is `/package.Service/Method`:

```json
{
  "request":  { "path": "/echo.v1.Echo/Say", "body": { "equalJson": { "message": "hi" } } },
  "response": { "jsonBody": { "message": "hello" } }
}
```

- `jsonBody` is encoded into the method's **output** message; `body` (raw string) is sent
  as an unframed payload if you need bytes.
- The request frame is decoded into ProtoJSON before matching, so `equalJson` on a gRPC
  stub works exactly like it does for HTTP. Field names follow ProtoJSON (lowerCamelCase).
- `grpcStatus` (default 0 = OK, and the standard 0-16 codes) plus `grpcMessage` return an
  error instead of a message.
- The mock's own codes: **12 UNIMPLEMENTED** when the method is not in the descriptor set
  (including when `-proto` was not passed), **5 NOT_FOUND** when no stub matched,
  **13 INTERNAL** for a malformed frame or a response body that does not fit the schema.
- The journal tags these rows `grpc` and shows the raw status code.

The **UI** datalists every rpc it knows on the path field, so gRPC stubs are authored the
same way as HTTP ones.

Cuts: gRPC-Web, compression, and multi-message server streaming. One reply frame is
produced per call, which is valid for unary and for a server stream of one message.
Client-streaming calls are matched on their first frame.

## Admin API

| Method | Path | Purpose |
|---|---|---|
| GET | `/__admin/health` | liveness + stub count |
| GET | `/__admin/mappings` | list stubs in match order |
| POST | `/__admin/mappings` | create a stub |
| PUT | `/__admin/mappings` | replace the whole list (bulk import; all-or-nothing) |
| GET/PUT/DELETE | `/__admin/mappings/{id}` | read / replace / delete |
| POST | `/__admin/mappings/{id}/move` | `{"direction":"up"}` or `"down"` |
| GET | `/__admin/requests?limit=100` | request journal, newest first |
| POST | `/__admin/verify` | `{"stubId":"..","expectedCount":1}` → `{"matched":n,"passed":bool}` |
| GET | `/__admin/grpc/methods` | every loaded rpc, as `/pkg.Service/Method` |
| GET | `/__admin/grpc/schema` | loaded services + per-folder warnings |
| POST | `/__admin/grpc/protos` | `{"files":{"path/to/x.proto":"<source>"}}` → compiles and replaces the schema |

`expectedCount` is optional; omitted means "at least one".

## Test it

```sh
curl -s -X POST localhost:8080/__admin/mappings -d '{
  "request":  {"method": "GET", "path": "/ping"},
  "response": {"status": 200, "body": "pong"}
}'

curl -si localhost:8081/ping          # 200 pong
curl -si localhost:8081/nope          # 404 no matching stub

curl -s -X POST localhost:8080/__admin/verify \
  -d '{"stubId":"<id from the POST above>"}'   # {"matched":1,"passed":true}
```

## Flags

| Flag | Default | Meaning |
|---|---|---|
| `-mock-addr` | `:8081` | address the app under test talks to |
| `-admin-addr` | `:8080` | admin API + UI (no auth — keep it on loopback) |
| `-stubs` | `stubs.json` | stub file; empty string disables persistence |
| `-proto` | *(empty)* | protobuf `FileDescriptorSet`; alternative to importing .proto in the UI |
| `-protos` | `protos.json` | cache of .proto files imported in the UI (empty disables) |
| `-journal-size` | `1000` | requests kept in the journal ring |

## Development

```sh
make test    # matcher table tests + HTTP and gRPC end-to-end httptest
make vet
make build
```

## Not built yet

Deliberate cuts, each worth adding only when a real need shows up: proxy passthrough to a
live upstream, fault injection, response templating, stateful scenarios / sequential
responses, the full WireMock matcher DSL (regex, jsonpath, xpath), journal clearing,
near-miss diffs on 404, CORS, TLS, WebSocket mocking, gRPC-Web, gRPC compression,
multi-message server streaming, record-and-playback.
