package main

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// tokenGuard requires `Authorization: Bearer <WHATSAPP_API_TOKEN>` on every
// request when a token is set. The loopback API otherwise answers any process
// on the machine: an app that runs the bridge for a member gives the token only
// to the MCP servers it starts, which keeps other accounts on the computer,
// sandboxed apps and web pages from reading chats or sending around them. It is
// not a sandbox: a program running as the same user can read the token from
// this process's environment. Unset, the API stays open, as it always was for
// a bridge started by hand.
type tokenGuard struct {
	next  http.Handler
	token []byte
}

func newTokenGuard(next http.Handler, token string) http.Handler {
	if token == "" {
		return next
	}
	return &tokenGuard{next: next, token: []byte(token)}
}

func (g *tokenGuard) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	presented, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || subtle.ConstantTimeCompare([]byte(presented), g.token) != 1 {
		writeJSON(w, http.StatusUnauthorized, errorResponse{
			Error:   "unauthorized",
			Details: "this bridge requires its API token; only the app that started it has it",
		})
		return
	}
	g.next.ServeHTTP(w, r)
}
