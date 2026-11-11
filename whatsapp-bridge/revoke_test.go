package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

// tickets#5656. Every revoke below is built by whatsmeow's own
// Client.BuildRevoke, on a client whose account is whoever sends it, and
// delivered through events.Message.UnwrapRaw: the proto a phone sends and the
// event the bridge receives, not a hand-made guess at either.

var (
	revokeMe     = types.NewJID("15550000001", types.DefaultUserServer)
	revokeClient = types.NewJID("573001112233", types.DefaultUserServer)
	revokeOther  = types.NewJID("573009998877", types.DefaultUserServer)
	revokeGroup  = types.NewJID("120363000000000001", types.GroupServer)
)

// revokeFrom is the message `account` sends to delete `id` for everyone in
// `chat` (the chat as the sender sees it). sender is the message's author when
// it is not the account's own (a group admin's delete).
func revokeFrom(account, chat, sender types.JID, id string) *waE2E.Message {
	own := account
	cli := whatsmeow.NewClient(&store.Device{ID: &own}, nil)
	return cli.BuildRevoke(chat, sender, id)
}

func revokeBridge(t *testing.T) (*Bridge, *sql.DB, string) {
	t.Helper()
	db := undecodedTestDB(t)
	media := t.TempDir()
	me := revokeMe
	return &Bridge{db: db, cfg: &Config{MediaPath: media}, client: whatsmeow.NewClient(&store.Device{ID: &me}, nil)}, db, media
}

// receive delivers a message the way whatsmeow does: the raw proto, unwrapped.
func receive(b *Bridge, chat, sender types.JID, fromMe bool, id string, ts int64, raw *waE2E.Message) {
	evt := (&events.Message{
		Info: types.MessageInfo{
			MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: fromMe, IsGroup: chat.Server == types.GroupServer},
			ID:            id,
			PushName:      "Cliente",
			Timestamp:     time.Unix(ts, 0),
		},
		RawMessage: raw,
	}).UnwrapRaw()
	b.handleEvent(evt)
}

type storedRow struct {
	deleted                         int
	typ                             string
	content, scrubbed, normal       sql.NullString
	transcript, mediaPath, mediaKey sql.NullString
}

func readRow(t *testing.T, db *sql.DB, id string) (storedRow, bool) {
	t.Helper()
	var r storedRow
	var key []byte
	err := db.QueryRow(`SELECT is_deleted, type, content_text, scrubbed_text, content_normalized, voice_note_transcript, media_path, media_key FROM messages WHERE id = ?`, id).
		Scan(&r.deleted, &r.typ, &r.content, &r.scrubbed, &r.normal, &r.transcript, &r.mediaPath, &key)
	if err == sql.ErrNoRows {
		return r, false
	}
	if err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	r.mediaKey = sql.NullString{String: string(key), Valid: key != nil}
	return r, true
}

func assertCleared(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	r, ok := readRow(t, db, id)
	if !ok {
		t.Fatalf("%s: the row should stay, cleared, so the message cannot come back", id)
	}
	if r.deleted != 1 || r.typ != "system" || r.content.Valid || r.scrubbed.Valid || r.normal.Valid || r.transcript.Valid || r.mediaPath.Valid || r.mediaKey.Valid {
		t.Fatalf("%s: a message deleted for everyone keeps nothing it said: %+v", id, r)
	}
}

func listMessages(t *testing.T, db *sql.DB, chat string) string {
	t.Helper()
	s := &Server{db: db, bridge: &Bridge{}}
	rec := httptest.NewRecorder()
	s.handleListMessages(rec, httptest.NewRequest(http.MethodGet, "/api/messages?chat_jid="+chat+"&limit=50", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/messages: %d %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func TestAClientsMessageDeletedForEveryoneIsGoneFromTheStoreTheAPIAndTheExport(t *testing.T) {
	b, db, _ := revokeBridge(t)
	chat := revokeClient.String()
	const secret = "mi cuenta es 123-456, olvida esto"
	receive(b, revokeClient, revokeClient, false, "M1", 1_790_000_000, &waE2E.Message{Conversation: proto.String(secret)})
	receive(b, revokeClient, revokeClient, false, "M2", 1_790_000_010, &waE2E.Message{Conversation: proto.String("¿Nos vemos el martes?")})
	receive(b, revokeClient, revokeMe, true, "M3", 1_790_000_020, &waE2E.Message{Conversation: proto.String("Sí, perfecto")})

	vault := t.TempDir()
	if err := ExportVault(db, vault, false, 1); err != nil {
		t.Fatalf("first export: %v", err)
	}
	if !strings.Contains(exportedText(t, vault), secret) {
		t.Fatalf("precondition: the message is exported before it is deleted")
	}

	// The client deletes it for everyone. From their side the chat is us.
	receive(b, revokeClient, revokeClient, false, "R1", 1_790_000_100, revokeFrom(revokeClient, revokeMe, types.EmptyJID, "M1"))

	assertCleared(t, db, "M1")
	if _, ok := readRow(t, db, "R1"); ok {
		t.Fatalf("the revoke itself is not a message and stores no row")
	}
	body := listMessages(t, db, chat)
	if strings.Contains(body, secret) || strings.Contains(body, `"M1"`) {
		t.Fatalf("/api/messages still serves the deleted message: %s", body)
	}
	if !strings.Contains(body, "¿Nos vemos el martes?") || !strings.Contains(body, "Sí, perfecto") {
		t.Fatalf("the rest of the chat stays: %s", body)
	}
	var last int64
	if err := db.QueryRow(`SELECT last_message_time FROM chats WHERE jid = ?`, chat).Scan(&last); err != nil || last != 1_790_000_100 {
		t.Fatalf("the deletion is the chat's newest activity, so readers look again: %d %v", last, err)
	}

	if err := ExportVault(db, vault, false, 1); err != nil {
		t.Fatalf("second export: %v", err)
	}
	out := exportedText(t, vault)
	if strings.Contains(out, secret) {
		t.Fatalf("the export still holds the deleted message:\n%s", out)
	}
	if !strings.Contains(out, "¿Nos vemos el martes?") {
		t.Fatalf("the export keeps the rest of the chat:\n%s", out)
	}
}

func exportedText(t *testing.T, dir string) string {
	t.Helper()
	var all strings.Builder
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.HasSuffix(p, ".md") {
			data, _ := os.ReadFile(p)
			all.Write(data)
		}
		return nil
	})
	return all.String()
}

func TestMyOwnDeleteForEveryoneFromMyPhoneClearsMyMessage(t *testing.T) {
	b, db, _ := revokeBridge(t)
	receive(b, revokeClient, revokeMe, true, "M1", 1_790_000_000, &waE2E.Message{Conversation: proto.String("precio equivocado: 900")})
	// My phone's own revoke reaches the linked bridge wrapped as device-sent.
	raw := &waE2E.Message{DeviceSentMessage: &waE2E.DeviceSentMessage{
		DestinationJID: proto.String(revokeClient.String()),
		Message:        revokeFrom(revokeMe, revokeClient, types.EmptyJID, "M1"),
	}}
	receive(b, revokeClient, revokeMe, true, "R1", 1_790_000_050, raw)
	assertCleared(t, db, "M1")
}

func TestADeletedVoiceNoteLosesItsTranscriptAndItsSavedAudio(t *testing.T) {
	b, db, media := revokeBridge(t)
	receive(b, revokeClient, revokeClient, false, "V1", 1_790_000_000, &waE2E.Message{AudioMessage: &waE2E.AudioMessage{
		PTT: proto.Bool(true), Mimetype: proto.String("audio/ogg"), MediaKey: []byte("k"), DirectPath: proto.String("/d"),
	}})
	file := filepath.Join(media, "V1.ogg")
	if err := os.WriteFile(file, []byte("audio"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE messages SET voice_note_transcript = 'lo que dije', media_path = ? WHERE id = 'V1'`, file); err != nil {
		t.Fatal(err)
	}
	receive(b, revokeClient, revokeClient, false, "R1", 1_790_000_030, revokeFrom(revokeClient, revokeMe, types.EmptyJID, "V1"))
	assertCleared(t, db, "V1")
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("the saved audio of a deleted voice note is removed: %v", err)
	}
	// A transcription that finishes after the delete writes nothing back.
	if _, err := db.Exec(`UPDATE messages SET voice_note_transcript = 'tarde' WHERE id = 'V1' AND voice_note_transcript IS NULL AND is_deleted = 0`); err != nil {
		t.Fatal(err)
	}
	assertCleared(t, db, "V1")
}

func TestAFileOutsideTheMediaFolderIsNeverRemoved(t *testing.T) {
	b, db, _ := revokeBridge(t)
	receive(b, revokeClient, revokeClient, false, "I1", 1_790_000_000, &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Caption: proto.String("foto")}})
	outside := filepath.Join(t.TempDir(), "not-ours.jpg")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE messages SET media_path = ? WHERE id = 'I1'`, outside); err != nil {
		t.Fatal(err)
	}
	receive(b, revokeClient, revokeClient, false, "R1", 1_790_000_030, revokeFrom(revokeClient, revokeMe, types.EmptyJID, "I1"))
	assertCleared(t, db, "I1")
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("a path outside the media folder is left alone: %v", err)
	}
}

func TestTheOtherSideCannotDeleteMyMessageInADirectChat(t *testing.T) {
	b, db, _ := revokeBridge(t)
	receive(b, revokeClient, revokeMe, true, "M1", 1_790_000_000, &waE2E.Message{Conversation: proto.String("lo que yo escribí")})
	// The client names my message in a revoke of their own.
	receive(b, revokeClient, revokeClient, false, "R1", 1_790_000_030, revokeFrom(revokeClient, revokeMe, types.EmptyJID, "M1"))
	if r, _ := readRow(t, db, "M1"); r.deleted != 0 || r.content.String != "lo que yo escribí" {
		t.Fatalf("only a message's own sender takes it back in a direct chat: %+v", r)
	}
}

func TestARevokeInOneChatCannotDeleteAMessageInAnother(t *testing.T) {
	b, db, _ := revokeBridge(t)
	receive(b, revokeOther, revokeOther, false, "M1", 1_790_000_000, &waE2E.Message{Conversation: proto.String("otro chat")})
	receive(b, revokeClient, revokeClient, false, "R1", 1_790_000_030, revokeFrom(revokeClient, revokeMe, types.EmptyJID, "M1"))
	if r, _ := readRow(t, db, "M1"); r.deleted != 0 || r.content.String != "otro chat" {
		t.Fatalf("a revoke only reaches its own chat: %+v", r)
	}
}

func TestAGroupAdminsDeleteClearsAnotherMembersMessage(t *testing.T) {
	b, db, _ := revokeBridge(t)
	receive(b, revokeGroup, revokeOther, false, "G1", 1_790_000_000, &waE2E.Message{Conversation: proto.String("spam")})
	receive(b, revokeGroup, revokeClient, false, "R1", 1_790_000_030, revokeFrom(revokeClient, revokeGroup, revokeOther, "G1"))
	assertCleared(t, db, "G1")
}

func TestARevokeThatArrivesFirstKeepsTheMessageFromBeingWrittenLater(t *testing.T) {
	b, db, _ := revokeBridge(t)
	receive(b, revokeClient, revokeClient, false, "R1", 1_790_000_100, revokeFrom(revokeClient, revokeMe, types.EmptyJID, "M1"))
	// The original arrives after, live and again in history, and a decode
	// backfill tries to recover it.
	text := &waE2E.Message{Conversation: proto.String("borrado antes de llegar")}
	receive(b, revokeClient, revokeClient, false, "M1", 1_790_000_000, text)
	b.processHistorySyncEvent(historyOf(revokeClient.String(), &waWeb.WebMessageInfo{
		Key:              revokeKey("M1", false),
		Message:          text,
		MessageTimestamp: proto.Uint64(1_790_000_000),
	}))
	if _, err := b.backfillDecodedContent("M1", text); err != nil {
		t.Fatal(err)
	}
	assertCleared(t, db, "M1")
	if strings.Contains(listMessages(t, db, revokeClient.String()), "borrado") {
		t.Fatalf("a message deleted before it arrived is never served")
	}
}

func TestHistorySaysAMessageWasRevoked(t *testing.T) {
	b, db, _ := revokeBridge(t)
	receive(b, revokeClient, revokeClient, false, "M1", 1_790_000_000, &waE2E.Message{Conversation: proto.String("se borrará")})
	b.processHistorySyncEvent(historyOf(revokeClient.String(), &waWeb.WebMessageInfo{
		Key:              revokeKey("M1", false),
		MessageStubType:  waWeb.WebMessageInfo_REVOKE.Enum(),
		MessageTimestamp: proto.Uint64(1_790_000_000),
	}))
	assertCleared(t, db, "M1")
}

func historyOf(chat string, msgs ...*waWeb.WebMessageInfo) *events.HistorySync {
	conv := &waHistorySync.Conversation{ID: proto.String(chat)}
	for _, m := range msgs {
		conv.Messages = append(conv.Messages, &waHistorySync.HistorySyncMsg{Message: m})
	}
	return &events.HistorySync{Data: &waHistorySync.HistorySync{Conversations: []*waHistorySync.Conversation{conv}}}
}

func revokeKey(id string, fromMe bool) *waCommon.MessageKey {
	return &waCommon.MessageKey{ID: proto.String(id), FromMe: proto.Bool(fromMe), RemoteJID: proto.String(revokeClient.String())}
}

// The API's JSON stays valid with deleted rows left out.
func TestTheAPIStillAnswersAChatWhoseOnlyMessageWasDeleted(t *testing.T) {
	b, db, _ := revokeBridge(t)
	receive(b, revokeClient, revokeClient, false, "M1", 1_790_000_000, &waE2E.Message{Conversation: proto.String("único")})
	receive(b, revokeClient, revokeClient, false, "R1", 1_790_000_030, revokeFrom(revokeClient, revokeMe, types.EmptyJID, "M1"))
	var out messageListResponse
	if err := json.Unmarshal([]byte(listMessages(t, db, revokeClient.String())), &out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 0 || len(out.Messages) != 0 {
		t.Fatalf("no message left to serve: %+v", out)
	}
}

func TestARevokeThatArrivesFirstInAnotherChatDoesNotSuppressTheMessage(t *testing.T) {
	b, db, _ := revokeBridge(t)
	// A revoke in the client's chat names an id the bridge has not seen.
	receive(b, revokeClient, revokeClient, false, "R1", 1_790_000_100, revokeFrom(revokeClient, revokeMe, types.EmptyJID, "M1"))
	// That id then arrives in another chat, live.
	receive(b, revokeOther, revokeOther, false, "M1", 1_790_000_000, &waE2E.Message{Conversation: proto.String("de otro chat")})
	if r, ok := readRow(t, db, "M1"); !ok || r.deleted != 0 || r.content.String != "de otro chat" {
		t.Fatalf("a revoke in one chat never suppresses a message in another: %+v", r)
	}
	// And the same through history.
	receive(b, revokeClient, revokeClient, false, "R2", 1_790_000_200, revokeFrom(revokeClient, revokeMe, types.EmptyJID, "M2"))
	b.processHistorySyncEvent(historyOf(revokeOther.String(), &waWeb.WebMessageInfo{
		Key:              &waCommon.MessageKey{ID: proto.String("M2"), FromMe: proto.Bool(false), RemoteJID: proto.String(revokeOther.String())},
		Message:          &waE2E.Message{Conversation: proto.String("historia de otro chat")},
		MessageTimestamp: proto.Uint64(1_790_000_000),
	}))
	if r, ok := readRow(t, db, "M2"); !ok || r.deleted != 0 || r.content.String != "historia de otro chat" {
		t.Fatalf("history of another chat is stored too: %+v", r)
	}
}

func TestTheOtherSideCannotDeleteMyMessageBeforeItIsStored(t *testing.T) {
	b, db, _ := revokeBridge(t)
	// The client revokes an id that turns out to be my own message.
	receive(b, revokeClient, revokeClient, false, "R1", 1_790_000_100, revokeFrom(revokeClient, revokeMe, types.EmptyJID, "M1"))
	receive(b, revokeClient, revokeMe, true, "M1", 1_790_000_000, &waE2E.Message{Conversation: proto.String("lo que yo escribí")})
	if r, ok := readRow(t, db, "M1"); !ok || r.deleted != 0 || r.content.String != "lo que yo escribí" {
		t.Fatalf("only a message's own side takes it back, even before it is stored: %+v", r)
	}
}

// editFrom is the message `account` sends to edit its own message `id`, built
// by whatsmeow's Client.BuildEdit.
func editFrom(account, chat types.JID, id, text string) *waE2E.Message {
	own := account
	cli := whatsmeow.NewClient(&store.Device{ID: &own}, nil)
	return cli.BuildEdit(chat, id, &waE2E.Message{Conversation: proto.String(text)})
}

func TestAnEditedMessageThenDeletedLeavesNeitherText(t *testing.T) {
	b, db, _ := revokeBridge(t)
	chat := revokeClient.String()
	receive(b, revokeClient, revokeClient, false, "M1", 1_790_000_000, &waE2E.Message{Conversation: proto.String("primera versión")})
	receive(b, revokeClient, revokeClient, false, "E1", 1_790_000_030, editFrom(revokeClient, revokeMe, "M1", "versión editada"))
	if r, _ := readRow(t, db, "M1"); r.content.String != "versión editada" {
		t.Fatalf("an edit changes the message it names: %+v", r)
	}
	if _, ok := readRow(t, db, "E1"); ok {
		t.Fatalf("an edit is not a message of its own")
	}
	receive(b, revokeClient, revokeClient, false, "R1", 1_790_000_060, revokeFrom(revokeClient, revokeMe, types.EmptyJID, "M1"))
	assertCleared(t, db, "M1")
	body := listMessages(t, db, chat)
	if strings.Contains(body, "versión") {
		t.Fatalf("neither version is served: %s", body)
	}
}

func TestOnlyAMessagesAuthorCanEditIt(t *testing.T) {
	b, db, _ := revokeBridge(t)
	receive(b, revokeClient, revokeMe, true, "M1", 1_790_000_000, &waE2E.Message{Conversation: proto.String("lo mío")})
	receive(b, revokeClient, revokeClient, false, "E1", 1_790_000_030, editFrom(revokeClient, revokeMe, "M1", "cambiado por otro"))
	if r, _ := readRow(t, db, "M1"); r.content.String != "lo mío" {
		t.Fatalf("the other side cannot edit my message: %+v", r)
	}
}

func TestThePreviewNeverShowsADeletedMessage(t *testing.T) {
	b, db, _ := revokeBridge(t)
	chat := revokeClient.String()
	// History creates the chat with a preview and no last_message_id.
	b.processHistorySyncEvent(historyOf(chat,
		&waWeb.WebMessageInfo{Key: revokeKey("H1", false), Message: &waE2E.Message{Conversation: proto.String("antes")}, MessageTimestamp: proto.Uint64(1_790_000_000)},
		&waWeb.WebMessageInfo{Key: revokeKey("H2", false), Message: &waE2E.Message{Conversation: proto.String("secreto en el preview")}, MessageTimestamp: proto.Uint64(1_790_000_010)},
	))
	receive(b, revokeClient, revokeClient, false, "R1", 1_790_000_060, revokeFrom(revokeClient, revokeMe, types.EmptyJID, "H2"))
	var preview sql.NullString
	if err := db.QueryRow(`SELECT last_message_preview FROM chats WHERE jid = ?`, chat).Scan(&preview); err != nil {
		t.Fatal(err)
	}
	if preview.String != "antes" {
		t.Fatalf("the preview is the newest message still there: %q", preview.String)
	}
}

func TestAStatusCanOnlyBeDeletedByItsOwnSender(t *testing.T) {
	b, db, _ := revokeBridge(t)
	status := types.StatusBroadcastJID
	receive(b, status, revokeOther, false, "S1", 1_790_000_000, &waE2E.Message{Conversation: proto.String("mi estado")})
	receive(b, status, revokeClient, false, "R1", 1_790_000_030, revokeFrom(revokeClient, status, revokeOther, "S1"))
	if r, _ := readRow(t, db, "S1"); r.deleted != 0 {
		t.Fatalf("another contact cannot delete someone's status: %+v", r)
	}
	receive(b, status, revokeOther, false, "R2", 1_790_000_040, revokeFrom(revokeOther, status, types.EmptyJID, "S1"))
	assertCleared(t, db, "S1")
}

func TestAnEditOfAMessageNotStoredYetIsDropped(t *testing.T) {
	b, db, _ := revokeBridge(t)
	// The client sends an "edit" naming my message M1 before M1 is stored.
	receive(b, revokeClient, revokeClient, false, "E1", 1_790_000_030, editFrom(revokeClient, revokeMe, "M1", "texto ajeno"))
	if _, ok := readRow(t, db, "M1"); ok {
		t.Fatalf("an edit of a message not stored writes nothing")
	}
	receive(b, revokeClient, revokeMe, true, "M1", 1_790_000_000, &waE2E.Message{Conversation: proto.String("lo que yo escribí")})
	if r, _ := readRow(t, db, "M1"); r.content.String != "lo que yo escribí" {
		t.Fatalf("my message is stored as I wrote it: %+v", r)
	}
}

func TestAnEditInHistoryLandsOnItsMessage(t *testing.T) {
	b, db, _ := revokeBridge(t)
	chat := revokeClient.String()
	receive(b, revokeClient, revokeClient, false, "M1", 1_790_000_000, &waE2E.Message{Conversation: proto.String("primera")})
	b.processHistorySyncEvent(historyOf(chat, &waWeb.WebMessageInfo{
		Key:              revokeKey("E1", false),
		Message:          editFrom(revokeClient, revokeMe, "M1", "editada en historia"),
		MessageTimestamp: proto.Uint64(1_790_000_030),
	}))
	if r, _ := readRow(t, db, "M1"); r.content.String != "editada en historia" {
		t.Fatalf("a history edit changes its message: %+v", r)
	}
	if _, ok := readRow(t, db, "E1"); ok {
		t.Fatalf("a history edit is not a message of its own")
	}
	receive(b, revokeClient, revokeClient, false, "R1", 1_790_000_060, revokeFrom(revokeClient, revokeMe, types.EmptyJID, "M1"))
	if strings.Contains(listMessages(t, db, chat), "editada") {
		t.Fatalf("the edited text goes with the delete")
	}
}

func TestAHistoryChunkListedNewestFirstAppliesItsEditAndRevoke(t *testing.T) {
	b, db, _ := revokeBridge(t)
	chat := revokeClient.String()
	b.processHistorySyncEvent(historyOf(chat,
		&waWeb.WebMessageInfo{Key: revokeKey("E2", false), Message: editFrom(revokeClient, revokeMe, "M2", "editado"), MessageTimestamp: proto.Uint64(1_790_000_040)},
		&waWeb.WebMessageInfo{Key: revokeKey("E1", false), Message: editFrom(revokeClient, revokeMe, "M1", "también editado"), MessageTimestamp: proto.Uint64(1_790_000_030)},
		&waWeb.WebMessageInfo{Key: revokeKey("M2", false), Message: &waE2E.Message{Conversation: proto.String("segundo")}, MessageTimestamp: proto.Uint64(1_790_000_010)},
		&waWeb.WebMessageInfo{Key: revokeKey("M1", false), Message: &waE2E.Message{Conversation: proto.String("primero")}, MessageTimestamp: proto.Uint64(1_790_000_000)},
	))
	if r, _ := readRow(t, db, "M2"); r.content.String != "editado" {
		t.Fatalf("an edit listed before its message still lands on it: %+v", r)
	}
	b.processHistorySyncEvent(historyOf(chat,
		&waWeb.WebMessageInfo{Key: revokeKey("R1", false), Message: revokeFrom(revokeClient, revokeMe, types.EmptyJID, "M1"), MessageTimestamp: proto.Uint64(1_790_000_050)},
	))
	assertCleared(t, db, "M1")
	body := listMessages(t, db, chat)
	if strings.Contains(body, "primero") || strings.Contains(body, "también editado") || !strings.Contains(body, "editado") {
		t.Fatalf("the edited message stays, the deleted one is gone: %s", body)
	}
}
