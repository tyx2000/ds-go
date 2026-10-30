package model

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestContextOverflowClassifiesBeforeRedaction(t *testing.T) {
	for _, status := range []int{400, 413} {
		for _, detail := range []string{"Input too long for this model", "context_window_exceeded", "This model's maximum context length is 100000 tokens"} {
			response := &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"type":"invalid_request_error","message":"` + detail + ` synthetic-secret-value"}}`))}
			err := New(Options{}).responseError(response)
			var failure *Error
			if !errors.As(err, &failure) || failure.Code != "CONTEXT_WINDOW_EXCEEDED" {
				t.Fatalf("%d %q => %v", status, detail, err)
			}
			if strings.Contains(err.Error(), "synthetic-secret-value") {
				t.Fatal("classification retained provider text")
			}
		}
	}
	for _, detail := range []string{"file not found", "request entity too large", "input token quota exceeded", "invalid model"} {
		err := New(Options{}).responseError(&http.Response{StatusCode: 400, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"` + detail + `"}}`))})
		var failure *Error
		if !errors.As(err, &failure) || failure.Code == "CONTEXT_WINDOW_EXCEEDED" {
			t.Fatalf("ordinary error became overflow: %v", err)
		}
	}
}
