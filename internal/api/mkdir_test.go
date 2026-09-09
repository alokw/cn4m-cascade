package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The validation in handleMakeDir runs before the target is ever loaded, so it
// is testable without a store or a mount manager — and it is the half that
// matters most: this endpoint creates directories, so a subpath that escapes
// the target root must be refused before anything touches the filesystem.
func TestMakeDirRejectsBadPaths(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantCode string
	}{
		{"empty path", `{"path":""}`, "invalid_path"},
		{"whitespace only", `{"path":"   "}`, "invalid_path"},
		{"absolute path", `{"path":"/etc"}`, "invalid_path"},
		{"parent traversal", `{"path":".."}`, "invalid_path"},
		{"traversal in the middle", `{"path":"a/../../etc"}`, "invalid_path"},
		{"unknown field", `{"pat":"x"}`, "invalid_body"},
		{"not json", `nope`, "invalid_body"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := testServer()
			req := httptest.NewRequest(http.MethodPost, "/api/targets/t1/mkdir", strings.NewReader(tc.body))
			req.SetPathValue("id", "t1")
			rec := httptest.NewRecorder()

			s.handleMakeDir(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d (body %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
			}
			var body errorBody
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("decoding error body: %v", err)
			}
			if body.Error.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", body.Error.Code, tc.wantCode)
			}
			if body.Error.Message == "" {
				t.Error("error message is empty; it is shown to the user")
			}
		})
	}
}
