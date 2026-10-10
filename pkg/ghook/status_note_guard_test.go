package ghook

import (
	"strings"
	"testing"
)

// gonk-l900: gonk now EDITS one status note on every issue it works on -- at
// RUN, at every re-sling, at defer/deny, and finally into the answer. If an
// edit of a gonk note reached the sink, every lifecycle step would be a fresh
// trigger and gonk would talk to itself. The loop guard keys on the numeric
// user id of the event, so an edit authored by the bot is dropped exactly as
// a create is -- even when the note body names the bot.
func TestBotEditedStatusNoteIsDropped(t *testing.T) {
	c := &capture{}
	h := newHandler(t, c) // BotUserID = 7, which is note-by-bot.json's author id
	body := strings.Replace(string(fixture(t, "note-by-bot")),
		`"note": "@gonk please re-triage this"`,
		`"note": "@gonk I am triaging this issue now (attempt 2, using model qwen3-6-35b).\n\n<!-- gonk:status:gonk:42:issue:3 -->", "action": "update"`, 1)
	if !strings.Contains(body, `"action": "update"`) {
		t.Fatal("fixture rewrite did not apply; the test would prove nothing")
	}
	w := post(t, h, "Note Hook", secretA, "application/json", body)
	if w.Code != 200 {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	if len(c.events) != 0 {
		t.Fatal("a bot-edited status note reached the sink: every lifecycle edit would retrigger gonk")
	}
	if c.outcomes[0] != OutcomeBotAuthored {
		t.Fatalf("outcome = %q, want bot_authored", c.outcomes[0])
	}
}
