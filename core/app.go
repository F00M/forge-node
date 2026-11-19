// The entry's HTTP surface: /healthz, git smart HTTP at /{owner}/{repo}.git, REST v3 at /api/v3,
// GraphQL at /api/graphql and MCP at /mcp, all behind the one bearer token.
package main

import (
	"regexp"
	"strings"
)

// The contest scenario's fixed viewer (wire.ts derives it from the fixed seed 0x6a09e667f3bcc909).
const viewerLogin = "adaiversen"
const viewerName = "Ada Iversen"
const viewerEmail = "adaiversen@example.invalid"

var otherAccounts = []accountSpec{
	{Login: "olgabauer", Type: "User"},
	{Login: "quinnkeller", Type: "User"},
	{Login: "faint-meadow-org", Type: "Organization"},
}

var healthBody = []byte(`{"ok":true}`)

func bearerToken(header string, present bool) (string, bool) {
	if !present {
		return "", false
	}
	value := strings.TrimSpace(header)
	space := strings.IndexByte(value, ' ')
	if space < 0 {
		return "", false
	}
	scheme := strings.ToLower(value[:space])
	credential := strings.TrimSpace(value[space+1:])
	if credential == "" {
		return "", false
	}
	switch scheme {
	case "bearer", "token":
		return credential, true
	case "basic":
		decoded := strings.ToValidUTF8(string(nodeBase64Decode(credential)), "�")
		colon := strings.IndexByte(decoded, ':')
		if colon < 0 {
			return decoded, decoded != ""
		}
		if password := decoded[colon+1:]; password != "" {
			return password, true
		}
		user := decoded[:colon]
		return user, user != ""
	}
	return "", false
}

var gitPathRe = regexp.MustCompile(`^/([^/]+)/([^/]+?)(?:\.git)?/(info/refs|git-upload-pack|git-receive-pack)$`)

type App struct {
	forge    *Forge
	views    *Views
	rest     *Rest
	smart    *SmartHTTP
	graphql  *GraphQL
	mcp      *Mcp
	expected string
}

func NewApp(token string) *App {
	f := NewForge(viewerLogin, viewerName, viewerEmail, otherAccounts)
	v := NewViews(f)
	return &App{
		forge:    f,
		views:    v,
		rest:     &Rest{forge: f, views: v},
		smart:    NewSmartHTTP(f),
		graphql:  NewGraphQL(f),
		mcp:      NewMcp(f, v),
		expected: token,
	}
}

func (a *App) authorized(req *Request) bool {
	h, ok := req.Headers["authorization"]
	token, ok := bearerToken(h, ok)
	if !ok {
		return false
	}
	return a.expected == "" || token == a.expected
}

func (a *App) setHost(req *Request) {
	if h, ok := req.Headers["host"]; ok {
		a.views.setHost(h)
	}
}

func (a *App) handle(req *Request) *Response {
	path := req.Path

	if path == "/healthz" {
		return jsonResp(200, healthBody, "")
	}

	if strings.HasPrefix(path, "/api/v3") {
		if len(path) > 7 && path[7] != '/' {
			return notFound()
		}
		a.setHost(req)
		if !a.authorized(req) {
			return errorJSON(401, "Requires authentication", nil)
		}
		rest := ""
		if len(path) > 8 {
			rest = path[8:]
		}
		var segments []string
		if rest != "" {
			segments = strings.Split(rest, "/")
		}
		if len(segments) > 0 && segments[len(segments)-1] == "" {
			segments = segments[:len(segments)-1]
		}
		return a.rest.handle(req, segments)
	}

	if path == "/api/graphql" || path == "/graphql" || path == "/api/graphql/" {
		a.setHost(req)
		if !a.authorized(req) {
			return jsonResp(401, O("message", "This endpoint requires you to be authenticated.", "documentation_url", "https://docs.github.com/graphql/guides/forming-calls-with-graphql#authenticating-with-graphql"), "")
		}
		if req.Method != "POST" && req.Method != "GET" {
			return errorJSON(405, "Method Not Allowed", nil)
		}
		var payload any
		if req.Method == "GET" {
			params := parseParams(req.Query)
			var variables any = map[string]any{}
			if v, err := parseJSON([]byte(params.GetOr("variables", "{}"))); err == nil {
				variables = v
			}
			var opName any
			if v, ok := params.Get("operationName"); ok {
				opName = v
			}
			payload = map[string]any{"query": params.GetOr("query", ""), "variables": variables, "operationName": opName}
		} else {
			v, err := parseJSON(req.Body)
			if err != nil {
				return errorJSON(400, "Problems parsing JSON", nil)
			}
			payload = v
		}
		origin := a.views.origin
		if list, ok := payload.([]any); ok {
			out := make([]any, len(list))
			for i, item := range list {
				m, ok := item.(map[string]any)
				if !ok {
					m = map[string]any{}
				}
				out[i] = a.graphql.execute(m, origin)
			}
			return jsonResp(200, out, "")
		}
		m, ok := payload.(map[string]any)
		if !ok {
			return errorJSON(400, "Problems parsing JSON", nil)
		}
		return jsonResp(200, a.graphql.execute(m, origin), "")
	}

	if path == "/mcp" || path == "/mcp/" {
		a.setHost(req)
		if !a.authorized(req) {
			return &Response{Status: 401, Type: jsonType, Body: toJSON(rpcError(nil, -32001, "Unauthorized: a bearer token is required")), Extra: "WWW-Authenticate: Bearer realm=\"forge\"\r\n"}
		}
		switch req.Method {
		case "GET":
			return &Response{Status: 405, Type: jsonType, Body: toJSON(rpcError(nil, -32000, "Method Not Allowed: this server does not offer an SSE stream")), Extra: "Allow: POST, DELETE\r\n"}
		case "DELETE":
			return &Response{Status: 204, Type: jsonType, Body: []byte{}}
		case "POST":
			status, body := a.mcp.handle(req.Body)
			return &Response{Status: status, Type: jsonType, Body: body}
		}
		return &Response{Status: 405, Type: jsonType, Body: toJSON(rpcError(nil, -32000, "Method Not Allowed")), Extra: "Allow: POST, DELETE\r\n"}
	}

	// Git smart HTTP: /{owner}/{repo}(.git)/info/refs, /git-upload-pack, /git-receive-pack.
	if m := gitPathRe.FindStringSubmatch(path); m != nil {
		if !a.authorized(req) {
			return &Response{Status: 401, Type: "text/plain; charset=utf-8", Body: []byte("Authentication required\n"), Extra: "WWW-Authenticate: Basic realm=\"forge\"\r\n"}
		}
		r := a.forge.visibleRepo(m[1], m[2])
		if r == nil {
			return textResp(404, "Repository not found\n")
		}
		action := m[3]
		protocol := req.Headers["git-protocol"]
		if action == "info/refs" {
			if req.Method != "GET" && req.Method != "HEAD" {
				return textResp(405, "Method Not Allowed\n")
			}
			service, _ := parseParams(req.Query).Get("service")
			if service != "git-upload-pack" && service != "git-receive-pack" {
				return textResp(403, "dumb http is not supported\n")
			}
			return a.smart.infoRefs(r, service, protocol)
		}
		if req.Method != "POST" {
			return textResp(405, "Method Not Allowed\n")
		}
		if action == "git-upload-pack" {
			resp, err := a.smart.uploadPack(r, req.Body, protocol)
			if err != nil {
				return textResp(400, err.Error()+"\n")
			}
			return resp
		}
		return a.smart.receivePack(r, req.Body)
	}

	if path == "/" || path == "" {
		return jsonResp(200, O("name", "forge", "api", "/api/v3", "graphql", "/api/graphql", "mcp", "/mcp"), "")
	}
	return notFound()
}
