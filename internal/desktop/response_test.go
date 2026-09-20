package desktop

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnexpectedDesktopFailureIsServerErrorWithoutInternalDetails(t *testing.T) {
	w := httptest.NewRecorder()
	respond(w, nil, errors.New("fixture database failure with private source details"))
	if w.Code != 500 || strings.Contains(w.Body.String(), "private source details") || !strings.Contains(w.Body.String(), "uncertain") {
		t.Fatalf("unsafe failure classification: %d %s", w.Code, w.Body.String())
	}
	for _, code := range []int{400, 403, 404, 409} {
		w = httptest.NewRecorder()
		respond(w, nil, fail(code, "explicit rejection"))
		if w.Code != code || !strings.Contains(w.Body.String(), "explicit rejection") {
			t.Fatalf("explicit rejection changed: %d %s", w.Code, w.Body.String())
		}
	}
}
