package db

import (
	"path/filepath"
	"testing"
	"time"
)

func TestMobileStoreQueries(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "mobile.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	sess, err := d.GetOrCreateSession("coachly", "gemini-3.8-flash-high", "normal")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := d.SaveChatMessage(ChatMessageRecord{
		SessionID:   sess.ID,
		ProjectName: "coachly",
		Role:        "user",
		Content:     "Explore Antigravity codebase",
	}); err != nil {
		t.Fatal(err)
	}

	sessions, err := d.ListSessions(20)
	if err != nil {
		t.Fatalf("ListSessions failed: %v", err)
	}
	if len(sessions) != 1 || sessions[0].Title != "Explore Antigravity codebase" {
		t.Fatalf("unexpected sessions: %+v", sessions)
	}

	msgs, err := d.ListSessionMessages(sess.ID, 20)
	if err != nil || len(msgs) != 1 {
		t.Fatalf("ListSessionMessages failed: %v, len=%d", err, len(msgs))
	}

	if _, _, err := d.EnqueueMessage(sess.ID, 1, "Run tests", ""); err != nil {
		t.Fatal(err)
	}
	qItems, err := d.ListPendingQueueItems(sess.ID)
	if err != nil || len(qItems) != 1 {
		t.Fatalf("ListPendingQueueItems failed: %v, len=%d", err, len(qItems))
	}

	if err := d.CreateApproval("app_1", sess.ID, "rm -rf build", time.Now().Add(5*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	apps, err := d.ListPendingApprovals()
	if err != nil || len(apps) != 1 {
		t.Fatalf("ListPendingApprovals failed: %v, len=%d", err, len(apps))
	}
}
