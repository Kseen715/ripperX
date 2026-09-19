package main

import (
	"encoding/json"
	"net/http"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
)

// The HTTP surface describes itself. A route carries both what the mux needs to
// serve an endpoint and what a reader needs to understand it, so the two cannot
// drift: routes.go is the only place an endpoint is declared, main registers
// from that table, and the OpenAPI document below is generated from the same
// table plus the Go types the handlers actually decode and encode.
//
// Field descriptions come from `doc:"..."` struct tags, and field names, types
// and optionality from the `json:"..."` tags the handlers already rely on. So a
// renamed field renames itself in the document, and a removed one disappears
// from it, without anyone remembering to look.
//
// This is deliberately not Swagger: the whole document is a few kilobytes built
// once on first request, and /docs renders it with the page's own stylesheet.

type route struct {
	// Method is enforced as well as documented; anything else gets 405.
	Method string
	// Extra are further methods the same pattern accepts. They are enforced
	// but not documented separately: an endpoint that takes two verbs says
	// so in its description.
	Extra   []string
	Pattern string // as ServeMux sees it - a trailing slash matches a prefix
	Handler http.HandlerFunc

	Summary string
	Desc    string

	// Param describes the path parameter: the {name} in the pattern, or the
	// trailing segment of a prefix pattern that ends in a slash.
	Param *param
	// Req and Resp are zero values of the request and success body types; the
	// schemas are read off them. Nil means no body.
	Req  any
	Resp any
	// Example is the body the docs page shows in its curl line. Without one it
	// shows the request type at its zero values, which is what the handlers
	// read as "use this server's defaults" - true of every endpoint here, but
	// not a useful thing to copy for the ones that take a rectangle.
	Example any
	// Code is the success status when it is not 200, and Type the success
	// content type when it is not JSON.
	Code int
	Type string
	// Other are the failure and no-content answers worth knowing about. They
	// carry an error body unless they are 2xx.
	Other []status
}

type param struct{ Name, Desc string }

type status struct {
	Code int
	Desc string
}

// serve wraps the handler in the method check the table promises.
func (rt route) serve() http.HandlerFunc {
	if rt.Method == "" {
		return rt.Handler
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != rt.Method && !(rt.Method == http.MethodGet && r.Method == http.MethodHead) {
			w.Header().Set("Allow", rt.Method)
			writeJSON(w, http.StatusMethodNotAllowed, errorResponse{Error: rt.Method + " only"})
			return
		}
		rt.Handler(w, r)
	}
}

// path is the pattern as OpenAPI names it. A pattern that already carries
// the mux's wildcards - "/api/drives/{id}/browse" - is one OpenAPI
// understands as it stands. A prefix pattern ending in a slash is really a
// path parameter, and gets the name spelled onto the end.
func (rt route) path() string {
	if rt.Param != nil && !strings.Contains(rt.Pattern, "{") {
		return strings.TrimSuffix(rt.Pattern, "/") + "/{" + rt.Param.Name + "}"
	}
	return rt.Pattern
}

// handleOpenAPI serves the generated document. It is built once and kept, which
// costs a few kilobytes and saves reflecting over every type per request.
func (s *server) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	s.specOnce.Do(func() {
		doc, err := json.Marshal(openAPI(s.api, s.authOn))
		if err != nil { // only a bug in the table can do this
			s.specErr = err
			return
		}
		s.spec = doc
	})
	if s.specErr != nil {
		writeErr(w, http.StatusInternalServerError, s.specErr)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache, must-revalidate")
	_, _ = w.Write(s.spec)
}

// handleDocs serves the page that renders the document above.
func (s *server) handleDocs(w http.ResponseWriter, r *http.Request) {
	http.ServeFileFS(w, r, s.web, "docs.html")
}

// openAPI builds the whole document from the route table.
func openAPI(routes []route, authOn bool) map[string]any {
	defs := map[string]any{}
	paths := map[string]any{}

	for i, rt := range routes {
		op := map[string]any{
			"summary":     rt.Summary,
			"operationId": operationID(rt),
			"responses":   responses(rt, defs),
			// Alphabetical paths would shuffle the reading order the table was
			// written in, so the page sorts on this instead.
			"x-order": i,
		}
		if rt.Desc != "" {
			op["description"] = rt.Desc
		}
		if rt.Param != nil {
			op["parameters"] = []any{map[string]any{
				"name":        rt.Param.Name,
				"in":          "path",
				"required":    true,
				"description": rt.Param.Desc,
				"schema":      map[string]any{"type": "string"},
			}}
		}
		if rt.Req != nil {
			body := map[string]any{"schema": schemaOf(reflect.TypeOf(rt.Req), defs)}
			if rt.Example != nil {
				body["example"] = rt.Example
			}
			op["requestBody"] = map[string]any{
				"required": false,
				"content":  map[string]any{"application/json": body},
			}
		}
		// Endpoints reachable without a token say so even when the server was
		// started without authentication, because that is a property of the
		// endpoint rather than of this run.
		if authOn && openPaths[rt.Pattern] {
			op["security"] = []any{}
		}

		p, _ := paths[rt.path()].(map[string]any)
		if p == nil {
			p = map[string]any{}
			paths[rt.path()] = p
		}
		p[strings.ToLower(rt.Method)] = op
	}

	components := map[string]any{"schemas": defs}
	doc := map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title": "escan",
			"description": "HTTP interface of the Epson Stylus CX4300 scanning server. " +
				"One scanner means one shared state: a scan started anywhere shows its " +
				"progress and its result everywhere, over /api/events.",
			"version": buildVersion(),
		},
		"servers":    []any{map[string]any{"url": "/"}},
		"paths":      paths,
		"components": components,
	}
	if authOn {
		components["securitySchemes"] = map[string]any{
			"bearerAuth": map[string]any{
				"type": "http", "scheme": "bearer",
				"description": "Access token from /api/login, as Authorization: Bearer <token>.",
			},
			"cookieAuth": map[string]any{
				"type": "apiKey", "in": "cookie", "name": sessionCookie,
				"description": "Set by /api/login; what a browser uses.",
			},
		}
		doc["security"] = []any{
			map[string]any{"bearerAuth": []any{}},
			map[string]any{"cookieAuth": []any{}},
		}
	}
	return doc
}

func operationID(rt route) string {
	id := strings.ToLower(rt.Method)
	for _, part := range strings.Split(strings.Trim(rt.path(), "/"), "/") {
		part = strings.NewReplacer("{", "", "}", "", ".", "-").Replace(part)
		for _, word := range strings.Split(part, "-") {
			if word != "" {
				id += strings.ToUpper(word[:1]) + word[1:]
			}
		}
	}
	return id
}

// responses lists what an endpoint answers: the success from Code/Type/Resp,
// and everything in Other, which carries an error body unless it is a 2xx.
func responses(rt route, defs map[string]any) map[string]any {
	out := map[string]any{}
	code := rt.Code
	if code == 0 {
		code = http.StatusOK
	}
	ok := map[string]any{"description": "success"}
	if rt.Resp != nil {
		ctype := rt.Type
		if ctype == "" {
			ctype = "application/json"
		}
		ok["content"] = map[string]any{
			ctype: map[string]any{"schema": schemaOf(reflect.TypeOf(rt.Resp), defs)},
		}
	} else if rt.Type != "" {
		ok["content"] = map[string]any{
			rt.Type: map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}},
		}
	}
	out[strconv.Itoa(code)] = ok

	for _, st := range rt.Other {
		r := map[string]any{"description": st.Desc}
		if st.Code >= 300 {
			r["content"] = map[string]any{
				"application/json": map[string]any{
					"schema": schemaOf(reflect.TypeOf(errorResponse{}), defs),
				},
			}
		}
		out[strconv.Itoa(st.Code)] = r
	}
	return out
}

// schemaOf returns the JSON Schema of t, registering every named struct it
// meets in defs and referring to it by name. Everything the document says about
// a body comes from here, so it says what the handler will actually accept.
func schemaOf(t reflect.Type, defs map[string]any) map[string]any {
	switch t.Kind() {
	case reflect.Pointer:
		// A nil pointer encodes as null, and the pages do rely on that - a nil
		// selection means the whole bed.
		s := schemaOf(t.Elem(), defs)
		if ref, ok := s["$ref"]; ok {
			return map[string]any{"oneOf": []any{map[string]any{"$ref": ref}, nullSchema()}}
		}
		s["nullable"] = true
		return s
	case reflect.Struct:
		if t.Name() == "" {
			return structSchema(t, defs)
		}
		if _, seen := defs[t.Name()]; !seen {
			// Reserve the name before descending, so a type that refers to
			// itself terminates.
			defs[t.Name()] = map[string]any{}
			defs[t.Name()] = structSchema(t, defs)
		}
		return map[string]any{"$ref": "#/components/schemas/" + t.Name()}
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 {
			return map[string]any{"type": "string", "format": "binary"}
		}
		return map[string]any{"type": "array", "items": schemaOf(t.Elem(), defs)}
	case reflect.Map:
		return map[string]any{"type": "object", "additionalProperties": schemaOf(t.Elem(), defs)}
	case reflect.Bool:
		return map[string]any{"type": "boolean"}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return map[string]any{"type": "integer"}
	case reflect.Float32, reflect.Float64:
		return map[string]any{"type": "number"}
	case reflect.String:
		return map[string]any{"type": "string"}
	default:
		// An interface field is any JSON value, which an empty schema says.
		return map[string]any{}
	}
}

func nullSchema() map[string]any { return map[string]any{"type": "null"} }

func structSchema(t reflect.Type, defs map[string]any) map[string]any {
	props := map[string]any{}
	var required, order []any
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported: encoding/json skips it, so do we
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" && opts == "" {
			continue
		}
		if f.Anonymous && name == "" {
			// An embedded struct's fields are the outer object's fields.
			inner := structSchema(f.Type, defs)
			for k, v := range inner["properties"].(map[string]any) {
				props[k] = v
			}
			if req, ok := inner["required"].([]any); ok {
				required = append(required, req...)
			}
			if ord, ok := inner["x-fieldOrder"].([]any); ok {
				order = append(order, ord...)
			}
			continue
		}
		if name == "" {
			name = f.Name
		}
		s := schemaOf(f.Type, defs)
		if doc := f.Tag.Get("doc"); doc != "" {
			// A $ref is the whole schema object, so a description beside it
			// needs its own wrapper to survive.
			if _, isRef := s["$ref"]; isRef {
				s = map[string]any{"allOf": []any{s}, "description": doc}
			} else {
				s["description"] = doc
			}
		}
		props[name] = s
		order = append(order, name)
		if !strings.Contains(opts, "omitempty") {
			required = append(required, name)
		}
	}
	out := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		out["required"] = required
	}
	// JSON objects come out of Go in alphabetical order, which scatters fields
	// that were written to be read in sequence. The order they are declared in
	// is kept here for the page to sort on.
	if len(order) > 0 {
		out["x-fieldOrder"] = order
	}
	return out
}

// buildVersion is whatever the build stamped into the binary; go build records
// the module version when there is one, and nothing useful for a local build.
func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "devel"
}
