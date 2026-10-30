//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSCOR019AttachmentFIFORefusedWithoutWriter(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "fifo", true: "symlink-to-fifo"}[symlink], func(t *testing.T) {
			a, session := lifecycleApp(t, lifecycleModel(nil))
			a.session = session
			fifo := filepath.Join(session.CWD, "pipe")
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			path := fifo
			if symlink {
				path = filepath.Join(session.CWD, "link")
				if err := os.Symlink(fifo, path); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan error, 1)
			go func() { done <- a.attach(path) }()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("special attachment was accepted")
				}
			case <-time.After(time.Second):
				// Unblock a regressed blocking open before ending the test.
				if fd, err := syscall.Open(fifo, syscall.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
					syscall.Close(fd)
				}
				select {
				case <-done:
				case <-time.After(time.Second):
				}
				t.Fatal("attachment open waited for a FIFO writer")
			}
			if a.hasDraftAttachments(session.ID) {
				t.Fatal("refused special file changed the draft")
			}
		})
	}
}

func TestSCOR019AttachmentDescriptorAcceptsRegularSymlinkAndRefusesDirectory(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, []byte("regular"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	opened, err := openAttachmentFile(link)
	if err != nil {
		t.Fatal(err)
	}
	opened.Close()
	if opened, err = openAttachmentFile(root); err == nil {
		opened.Close()
		t.Fatal("directory descriptor was accepted")
	}
}
