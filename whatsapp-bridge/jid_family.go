package main

import (
	"context"
	"net/http"
	"strings"

	"go.mau.fi/whatsmeow/types"
)

// jidFamily is every JID this bridge knows for the same chat: the JID itself
// without a device part, the alternates in jid_aliases, and the one
// whatsmeow's own LID store knows (it learns them before jid_aliases is
// backfilled). One person is one chat under their phone JID and their LID,
// so an app that splits chats between workspaces checks the whole family.
func (s *Server) jidFamily(ctx context.Context, jid string) []string {
	base := stripDevicePart(jid)
	family, _ := resolveAliases(ctx, s.db, base)
	if len(family) == 0 {
		family = []string{base}
	}
	for _, known := range append([]string(nil), family...) {
		family = appendNew(family, s.altJIDs(ctx, known)...)
	}
	return family
}

// stripDevicePart turns "573001234567:12@s.whatsapp.net" into
// "573001234567@s.whatsapp.net": the device is not a different chat.
func stripDevicePart(jid string) string {
	if parsed, err := types.ParseJID(jid); err == nil {
		return parsed.ToNonAD().String()
	}
	if user, server, ok := strings.Cut(jid, "@"); ok {
		user, _, _ = strings.Cut(user, ":")
		return user + "@" + server
	}
	return jid
}

// handleAliases is GET /api/aliases?jid=X: X's whole family, for a client
// that must treat one person's JIDs as one chat (an MCP server scoped to a
// workspace).
func (s *Server) handleAliases(w http.ResponseWriter, r *http.Request) {
	jid := r.URL.Query().Get("jid")
	if jid == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "jid required"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"jid": jid, "family": s.jidFamily(r.Context(), jid)})
}
