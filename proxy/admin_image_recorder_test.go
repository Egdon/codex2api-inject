package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
)

// Offline regression sources. No upstream requests are needed.
func TestAdminImageRecorderInformationalFinalResponse(t *testing.T) {
	for _, status := range []int{200, 400, 429, 500} {
		r := newAdminImageRecorder()
		c, _ := gin.CreateTestContext(r)
		underlying, ok := unwrapHTTPResponseWriter(c.Writer)
		if !ok {
			t.Fatal("Gin writer must unwrap to adapter")
		}
		underlying.WriteHeader(102)
		underlying.WriteHeader(103)
		underlying.WriteHeader(102)
		c.JSON(status, gin.H{"message": "fixture"})
		body, code, err := r.imageResult("generation")
		if code != status || r.Code != status {
			t.Fatalf("got %d/%d want %d", code, r.Code, status)
		}
		if (err == nil) != (status == 200) {
			t.Fatalf("status %d error %v", status, err)
		}
		if !bytes.Contains(body, []byte("fixture")) {
			t.Fatal("final body lost")
		}
	}
}

func TestAdminImageRecorderRequiresFinalResponse(t *testing.T) {
	r := newAdminImageRecorder()
	r.WriteHeader(102)
	if _, status, err := r.imageResult("edit"); status != 502 || err == nil {
		t.Fatal("interim-only response accepted")
	}
	r.WriteHeader(101)
	if _, status, err := r.imageResult("edit"); status != 101 || err == nil {
		t.Fatal("upgrade accepted as image")
	}
}

func TestAdminImageRecorderImplicitWrites(t *testing.T) {
	for _, write := range []func(*adminImageRecorder){
		func(r *adminImageRecorder) { _, _ = r.Write([]byte("fixture")) },
		func(r *adminImageRecorder) { _, _ = io.WriteString(r, "fixture") },
		func(r *adminImageRecorder) { r.Flush() },
	} {
		r := newAdminImageRecorder()
		r.WriteHeader(102)
		write(r)
		r.WriteHeader(500) // The final response is already committed.
		if _, code, err := r.imageResult("generation"); code != 200 || err != nil {
			t.Fatalf("implicit response %d %v", code, err)
		}
	}
}

func TestAdminImageErrorsExcludeImageBodiesAndBoundText(t *testing.T) {
	for _, body := range []string{
		`{"created":1,"data":[{"b64_json":"fixture-image"}]}`,
		`{"data":[{"url":"data:image/png;base64,fixture-image"}]}`,
		strings.Repeat("fixture-image", 10000),
	} {
		message := extractAdminImageErrorMessage([]byte(body))
		if strings.Contains(message, "fixture-image") {
			t.Fatal("image payload echoed")
		}
	}
	if message := extractAdminImageErrorMessage([]byte(`{"error":{"message":"quota exceeded"}}`)); message != "quota exceeded" {
		t.Fatal(message)
	}
	message := extractAdminImageErrorMessage([]byte(strings.Repeat("错误", 1000)))
	if len(message) > maxAdminImageErrorBytes+3 || !utf8.ValidString(message) {
		t.Fatal("unbounded or broken UTF8")
	}
}

var _ http.ResponseWriter = (*adminImageRecorder)(nil)
var _ http.Flusher = (*adminImageRecorder)(nil)
var _ io.StringWriter = (*adminImageRecorder)(nil)
