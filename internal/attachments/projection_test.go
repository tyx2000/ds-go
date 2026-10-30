package attachments

import "testing"

func TestAttachmentHandlePreservesLiteralEscapesAndUnicode(t *testing.T) {
	for _, item := range []struct{ name, quoted string }{
		{`literal \u2028`, `"literal \\u2028"`},
		{`literal \u2029`, `"literal \\u2029"`},
		{"separator \u2028\u2029", "\"separator \u2028\u2029\""},
		{`quote " and <&>`, `"quote \" and <&>"`},
	} {
		if got := quotedAttachment(item.name); got != item.quoted {
			t.Errorf("name %q quoted as %q, want %q", item.name, got, item.quoted)
		}
	}
}
