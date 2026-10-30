package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"dsgo/internal/attachments"
	"dsgo/internal/core"
)

func TestSCOR010DraftAttachmentsRemainWithTheirSession(t *testing.T) {
	for _, fresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "resume", true: "new"}[fresh], func(t *testing.T) {
			a, first := lifecycleApp(t, lifecycleModel(func(context.Context, core.Request, func(core.Delta)) (core.Response, error) {
				return core.Response{Content: []core.Block{{Type: "text", Text: "done"}}}, nil
			}))
			a.session, a.flags.cwd = first, first.CWD
			path := filepath.Join(first.CWD, "private-draft.txt")
			if err := os.WriteFile(path, []byte("PRIVATE_TO_FIRST_SESSION"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.attach(path); err != nil {
				t.Fatal(err)
			}
			next := ""
			if !fresh {
				second, err := a.store.Create(t.TempDir(), "second")
				if err != nil {
					t.Fatal(err)
				}
				next = second.ID
			}
			if err := a.selectSession(next); err != nil {
				t.Fatal(err)
			}
			second := a.session.ID
			if a.hasDraftAttachments(second) || !a.hasDraftAttachments(first.ID) {
				t.Fatal("Session switch transferred the first Session's draft")
			}
			if err := a.addDraftAttachment(second, core.Block{Type: "text", Text: "PRIVATE_TO_SECOND_SESSION"}, 0); err != nil {
				t.Fatal(err)
			}
			if err := a.turn("second Session message"); err != nil {
				t.Fatal(err)
			}
			messages, err := a.store.Messages(second)
			if err != nil {
				t.Fatal(err)
			}
			projection := string(core.JSON(messages))
			if strings.Contains(projection, "PRIVATE_TO_FIRST_SESSION") || !strings.Contains(projection, "PRIVATE_TO_SECOND_SESSION") {
				t.Fatalf("second Session got the wrong draft: %s", projection)
			}
			if a.hasDraftAttachments(second) || !a.hasDraftAttachments(first.ID) {
				t.Fatal("second admission retired another Session's pending draft")
			}
			if err := a.selectSession(first.ID); err != nil {
				t.Fatal(err)
			}
			if err := a.turn("first Session message"); err != nil {
				t.Fatal(err)
			}
			messages, err = a.store.Messages(first.ID)
			if err != nil {
				t.Fatal(err)
			}
			projection = string(core.JSON(messages))
			if !strings.Contains(projection, "PRIVATE_TO_FIRST_SESSION") || strings.Contains(projection, "PRIVATE_TO_SECOND_SESSION") || a.hasDraftAttachments(first.ID) {
				t.Fatalf("first Session did not recover and retire its own draft: %s", projection)
			}
		})
	}
}

func TestSCOR010FailedSwitchAndOlderAdmissionPreserveDrafts(t *testing.T) {
	a, session := lifecycleApp(t, lifecycleModel(nil))
	a.session = session
	if err := a.addDraftAttachment(session.ID, core.Block{Type: "text", Text: "first"}, 0); err != nil {
		t.Fatal(err)
	}
	if err := a.selectSession("missing-session"); err == nil {
		t.Fatal("expected missing Session error")
	}
	if a.session.ID != session.ID || len(a.draftAttachments(session.ID)) != 1 {
		t.Fatal("failed switch lost the current Session's draft")
	}
	_, through := a.snapshotDraftAttachments(session.ID)
	if err := a.addDraftAttachment(session.ID, core.Block{Type: "text", Text: "later"}, 0); err != nil {
		t.Fatal(err)
	}
	a.retireDraftAttachments(session.ID, through)
	remaining := a.draftAttachments(session.ID)
	if len(remaining) != 1 || remaining[0].Text != "later" {
		t.Fatalf("older admission retired a later draft: %+v", remaining)
	}
}

func TestSCOR018AttachmentUsesCurrentWorkingDirectory(t *testing.T) {
	a, session := lifecycleApp(t, lifecycleModel(nil))
	a.session = session
	child := filepath.Join(session.CWD, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	for path, contents := range map[string]string{filepath.Join(session.CWD, "same.txt"): "WRONG_ROOT_FILE", filepath.Join(child, "same.txt"): "RIGHT_CHILD_FILE"} {
		if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.workingDirectory(a.ctx, core.ToolContext{SessionID: session.ID}, core.JSON(map[string]any{"cd": "child"})); err != nil {
		t.Fatal(err)
	}
	if err := a.attach("same.txt"); err != nil {
		t.Fatal(err)
	}
	draft := a.draftAttachments(session.ID)
	if len(draft) != 1 || !strings.Contains(draft[0].Text, "RIGHT_CHILD_FILE") || strings.Contains(draft[0].Text, "WRONG_ROOT_FILE") {
		t.Fatalf("relative attachment resolved against the original workspace: %+v", draft)
	}
}

func TestSCOR026DraftImageBatchUsesRawAdmissionBytes(t *testing.T) {
	a, session := lifecycleApp(t, lifecycleModel(nil))
	a.session = session
	// Ten legal raw uploads fill 200 MiB even when each stored, normalized
	// reference is tiny. No 200 MiB fixture needs to be decoded for this case.
	image := core.Block{Type: "image", Source: map[string]any{"bytes": 1}}
	for range 10 {
		if err := a.addDraftAttachment(session.ID, image, attachments.MaxImageBytes); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(session.CWD, "next.png")
	if err := os.WriteFile(path, []byte("\x89PNG\r\n\x1a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.attach(path); err == nil || !strings.Contains(err.Error(), "200 MiB") {
		t.Fatalf("attachment did not refuse the full raw batch before route lookup: %v", err)
	}
	if len(a.draftAttachments(session.ID)) != 10 {
		t.Fatal("refused upload changed the draft")
	}
	if err := a.checkDraftImageLimit("independent-session", attachments.MaxImageBytes); err != nil {
		t.Fatalf("raw-byte budget leaked to another Session: %v", err)
	}
	_, through := a.snapshotDraftAttachments(session.ID)
	a.retireDraftAttachments(session.ID, through)
	if err := a.checkDraftImageLimit(session.ID, attachments.MaxImageBytes); err != nil {
		t.Fatalf("admitted batch kept charging the next draft: %v", err)
	}
}

func TestSCOR026DraftImageCountAndRawByteBoundary(t *testing.T) {
	var a App
	image := core.Block{Type: "image", Source: map[string]any{"bytes": 1}}
	for range attachments.MaxImagesPerMessage {
		if err := a.addDraftAttachment("session", image, attachments.MaxMessageImageBytes/attachments.MaxImagesPerMessage); err != nil {
			t.Fatal("exact official count and byte boundary refused:", err)
		}
	}
	if err := a.addDraftAttachment("session", image, 1); err == nil {
		t.Fatal("21st image was accepted")
	}
}

func TestSCOR019AttachmentReadRejectsOversizeAndCancellation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large.txt")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = file.Truncate(attachments.MaxImageBytes + 1); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()
	if _, err = readAttachmentFile(context.Background(), path); err == nil {
		t.Fatal("oversized attachment was read")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = readAttachmentFile(ctx, "missing-file"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read opened its pathname: %v", err)
	}
}
