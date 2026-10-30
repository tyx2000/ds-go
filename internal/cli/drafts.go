package cli

import (
	"errors"
	"sync"

	"dsgo/internal/attachments"
	"dsgo/internal/core"
)

// Drafts belong to Session input scopes, as in the official InputHub. Their
// admission metadata stays local: raw image bytes must not be confused with a
// durable reference's normalized byte count.
type attachmentDrafts struct {
	mu       sync.Mutex
	next     uint64
	sessions map[string][]draftAttachment
}

type draftAttachment struct {
	sequence      uint64
	block         core.Block
	rawImageBytes int
}

func draftImageLimit(entries []draftAttachment, rawBytes int) error {
	count, total := 0, 0
	for _, entry := range entries {
		if entry.block.Type == "image" {
			count++
			total += entry.rawImageBytes
		}
	}
	if rawBytes < 0 || rawBytes > attachments.MaxImageBytes || count >= attachments.MaxImagesPerMessage || rawBytes > attachments.MaxMessageImageBytes-total {
		return errors.New("message image attachments exceed 20 images or 200 MiB")
	}
	return nil
}

func (a *App) checkDraftImageLimit(id string, rawBytes int) error {
	a.drafts.mu.Lock()
	defer a.drafts.mu.Unlock()
	return draftImageLimit(a.drafts.sessions[id], rawBytes)
}

func (a *App) addDraftAttachment(id string, block core.Block, rawImageBytes int) error {
	a.drafts.mu.Lock()
	defer a.drafts.mu.Unlock()
	if block.Type == "image" {
		if err := draftImageLimit(a.drafts.sessions[id], rawImageBytes); err != nil {
			return err
		}
	}
	if a.drafts.sessions == nil {
		a.drafts.sessions = make(map[string][]draftAttachment)
	}
	a.drafts.next++
	a.drafts.sessions[id] = append(a.drafts.sessions[id], draftAttachment{sequence: a.drafts.next, block: block, rawImageBytes: rawImageBytes})
	return nil
}

func (a *App) snapshotDraftAttachments(id string) ([]core.Block, uint64) {
	a.drafts.mu.Lock()
	defer a.drafts.mu.Unlock()
	entries := a.drafts.sessions[id]
	if len(entries) == 0 {
		return nil, 0
	}
	blocks := make([]core.Block, len(entries))
	for i, entry := range entries {
		blocks[i] = entry.block
	}
	return blocks, entries[len(entries)-1].sequence
}

func (a *App) draftAttachments(id string) []core.Block {
	blocks, _ := a.snapshotDraftAttachments(id)
	return blocks
}

func (a *App) hasDraftAttachments(id string) bool {
	a.drafts.mu.Lock()
	defer a.drafts.mu.Unlock()
	return len(a.drafts.sessions[id]) != 0
}

// Only the submitted prefix retires. A later draft addition cannot be removed
// by an older submission's admission or error settlement.
func (a *App) retireDraftAttachments(id string, through uint64) {
	a.drafts.mu.Lock()
	defer a.drafts.mu.Unlock()
	entries := a.drafts.sessions[id]
	first := 0
	for first < len(entries) && entries[first].sequence <= through {
		first++
	}
	if first == len(entries) {
		delete(a.drafts.sessions, id)
	} else if first > 0 {
		a.drafts.sessions[id] = append([]draftAttachment(nil), entries[first:]...)
	}
}
